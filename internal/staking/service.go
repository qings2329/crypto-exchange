package staking

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"go.uber.org/zap"
)

// 链上解质押所需的确认数阈值（生产级应可配置；此处取保守值）。
const requiredConfirmations = int64(12)

// ChainBackend 是链上质押后端的可插拔抽象。staking 不直接耦合具体链的签名/广播细节，
// 由后端实现（MockBackend 用于演示/测试，TronBackend 复用 settlement 链上 RPC）。
type ChainBackend interface {
	// Stake 广播一笔链上质押交易，返回交易哈希。
	Stake(ctx context.Context, chain, contractAddr, delegator, validator string, amount settlement.AssetAmount) (txHash string, err error)
	// Unbond 广播解质押交易，返回交易哈希。
	Unbond(ctx context.Context, chain, contractAddr, delegator, validator string, amount settlement.AssetAmount) (txHash string, err error)
	// PendingReward 返回指定委托者当前链上待领取的质押奖励额（定点）。
	PendingReward(ctx context.Context, chain, contractAddr, delegator string) (settlement.AssetAmount, error)
	// Confirmations 返回某交易当前的链上确认数。
	Confirmations(ctx context.Context, chain, txHash string) (int64, error)
}

// Config 是质押服务配置。
type Config struct {
	AccrueInterval time.Duration // 后台奖励归集周期
}

// Service 质押业务服务。
type Service struct {
	store   Store
	ledger  *ledger.Ledger
	backend ChainBackend
	cfg     Config
	log     *zap.Logger
}

// NewService 构造质押服务。backend 为 nil 时退化为 MockBackend（演示/测试）。
func NewService(store Store, l *ledger.Ledger, backend ChainBackend, cfg Config, log *zap.Logger) *Service {
	if backend == nil {
		backend = NewMockBackend()
	}
	return &Service{store: store, ledger: l, backend: backend, cfg: cfg, log: log}
}

// Subscribe 用户委托质押：校验 -> 账本锁定本金(SysStaking) -> 链上质押广播 -> 落委托。
// F2 定点：金额全 AssetAmount；F5 校验：正数/起质押额/资产白名单；F3 原子：锁本金与链上广播
// 任一失败均回退账本（方向相反，指纹不同，不会被账本幂等去重误吞）。
func (s *Service) Subscribe(userID, productID int64, amount settlement.AssetAmount) (*StakingDelegation, error) {
	if amount.Sign() <= 0 {
		return nil, ErrInvalidAmount
	}
	p, err := s.store.GetProduct(productID)
	if err != nil {
		return nil, err
	}
	if p.Status != ProductActive {
		return nil, ErrProductNotFound
	}
	if !settlement.KnownAsset(p.Asset) {
		return nil, ErrUnsupportedAsset
	}
	// 起质押额在定点空间比较（无 1e-9 浮点容差）。
	if amount.Cmp(p.MinAmount) < 0 {
		return nil, ErrBelowMinAmount
	}

	ref := fmt.Sprintf("stake:%d:%d", userID, productID)
	// 1) 账本锁定本金。
	if err := s.ledger.Transfer(userID, ledger.SysStaking, p.Asset, amount, "stake_lock", ref); err != nil {
		return nil, err
	}
	// 2) 链上质押广播。
	txHash, err := s.backend.Stake(context.Background(), p.Chain, p.ContractAddr,
		fmt.Sprintf("%d", userID), p.Validator, amount)
	if err != nil {
		// 回退锁定的本金（方向相反，指纹不同，不被去重）。
		_ = s.ledger.Transfer(ledger.SysStaking, userID, p.Asset, amount, "stake_lock_revert", ref)
		return nil, fmt.Errorf("stake on chain: %w", err)
	}
	// 3) 落委托。
	d := &StakingDelegation{
		UserID:    userID,
		ProductID: productID,
		Principal:  amount,
		Status:     DelegationActive,
		TxHash:     txHash,
		CreatedAt:  time.Now().Unix(),
	}
	if err := s.store.CreateDelegation(d); err != nil {
		// 委托落库失败：回退本金。
		_ = s.ledger.Transfer(ledger.SysStaking, userID, p.Asset, amount, "stake_lock_revert", ref)
		return nil, err
	}
	return d, nil
}

// Unbond 发起解质押：校验归属/终态 -> 链上解质押广播 -> 标记 unbonding（解锁排队）。
// F1 幂等：已 unbonding/unbonded 终态短路，避免重复广播双付。
func (s *Service) Unbond(userID, delegationID int64) (*StakingDelegation, error) {
	d, err := s.store.GetDelegation(delegationID)
	if err != nil {
		return nil, err
	}
	if d.UserID != userID {
		return nil, ErrNotOwner
	}
	if d.Status != DelegationActive {
		return nil, ErrAlreadyUnbonded
	}
	p, err := s.store.GetProduct(d.ProductID)
	if err != nil {
		return nil, err
	}
	txHash, err := s.backend.Unbond(context.Background(), p.Chain, p.ContractAddr,
		fmt.Sprintf("%d", userID), p.Validator, d.Principal)
	if err != nil {
		return nil, fmt.Errorf("unbond on chain: %w", err)
	}
	d.Status = DelegationUnbonding
	d.TxHash = txHash
	d.UnbondAt = time.Now().Unix()
	if err := s.store.UpdateDelegation(d); err != nil {
		return nil, err
	}
	return d, nil
}

// Release 解质押确认后释放：链上确认数达标 -> 本金+累计奖励经 ledger.Batch 原子释放给用户。
// F3 原子：本金与奖励两笔转账整体执行，失败整组回滚。
func (s *Service) Release(userID, delegationID int64) (*StakingDelegation, error) {
	d, err := s.store.GetDelegation(delegationID)
	if err != nil {
		return nil, err
	}
	if d.UserID != userID {
		return nil, ErrNotOwner
	}
	if d.Status == DelegationUnbonded {
		return nil, ErrAlreadyUnbonded
	}
	p, err := s.store.GetProduct(d.ProductID)
	if err != nil {
		return nil, err
	}
	conf, err := s.backend.Confirmations(context.Background(), p.Chain, d.TxHash)
	if err != nil {
		return nil, err
	}
	if conf < requiredConfirmations {
		return nil, ErrUnbondPending
	}
	// 累计链上已归集奖励（记录在 SysStakingReward 的负债）。
	rewards, _ := s.store.ListRewardsByDelegation(d.ID)
	totalReward := settlement.NewAssetAmount(big.NewInt(0), d.Principal.Decimals)
	for _, r := range rewards {
		totalReward = totalReward.Add(r.Amount)
	}
	ref := fmt.Sprintf("unbond_release:%d", d.ID)
	ops := []ledger.Op{{
		Kind: ledger.OpTransfer, From: ledger.SysStaking, To: userID,
		Asset: p.Asset, Amount: d.Principal, Biz: "stake_release_principal", Ref: ref,
	}}
	if totalReward.Sign() > 0 {
		ops = append(ops, ledger.Op{
			Kind: ledger.OpTransfer, From: ledger.SysStakingReward, To: userID,
			Asset: p.Asset, Amount: totalReward, Biz: "stake_release_reward", Ref: ref,
		})
	}
	if err := s.ledger.Batch(ops); err != nil {
		return nil, err
	}
	d.Status = DelegationUnbonded
	d.UnbondedAt = time.Now().Unix()
	if err := s.store.UpdateDelegation(d); err != nil {
		return nil, err
	}
	return d, nil
}

// Accrue 后台奖励归集：遍历 active 委托，查询链上待领取奖励，按 F2 定点经账本发放到
// SysStakingReward（平台对用户欠付的奖励负债），并落奖励记录。返回本轮归集总额。
func (s *Service) Accrue(now time.Time) (settlement.AssetAmount, error) {
	total := settlement.NewAssetAmount(big.NewInt(0), 8)
	delegations, err := s.store.ListAllDelegations()
	if err != nil {
		return total, err
	}
	for _, d := range delegations {
		if d.Status != DelegationActive {
			continue
		}
		p, err := s.store.GetProduct(d.ProductID)
		if err != nil || p.Status != ProductActive {
			continue
		}
		rew, err := s.backend.PendingReward(context.Background(), p.Chain, p.ContractAddr,
			fmt.Sprintf("%d", d.UserID))
		if err != nil || rew.Sign() <= 0 {
			continue
		}
		// F2 定点 + F3 原子：SysStaking -> SysStakingReward（复式，余额恒等）。
		ref := fmt.Sprintf("stake_accrue:%d:%d", d.ID, now.Unix())
		if err := s.ledger.Transfer(ledger.SysStaking, ledger.SysStakingReward, p.Asset, rew,
			"stake_accrue", ref); err != nil {
			continue
		}
		r := &StakingReward{DelegationID: d.ID, Amount: rew, AccruedAt: now.Unix()}
		_ = s.store.CreateReward(r)
		total = total.Add(rew)
	}
	return total, nil
}

// RunLoop 后台奖励归集循环（ticker 驱动），ctx 取消即退出。
func (s *Service) RunLoop(ctx context.Context) {
	if s.cfg.AccrueInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.cfg.AccrueInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Accrue(time.Now()); err != nil && s.log != nil {
				s.log.Warn("stake accrue failed", zap.Error(err))
			}
		}
	}
}

// ---- MockBackend：演示/测试用，不实际广播链上交易 ----

// MockBackend 是 ChainBackend 的内存实现，用于无节点环境与单测。
type MockBackend struct{}

// NewMockBackend 构造 Mock 后端。
func NewMockBackend() *MockBackend { return &MockBackend{} }

func (m *MockBackend) Stake(_ context.Context, _chain, _contract, delegator, _validator string, amount settlement.AssetAmount) (string, error) {
	return fmt.Sprintf("0xmockstake_%s_%s", delegator, amount.HumanString()), nil
}

func (m *MockBackend) Unbond(_ context.Context, _chain, _contract, delegator, _validator string, _ settlement.AssetAmount) (string, error) {
	return fmt.Sprintf("0xmockunbond_%s", delegator), nil
}

func (m *MockBackend) PendingReward(_ context.Context, _chain, _contract, _delegator string) (settlement.AssetAmount, error) {
	// 模拟每轮固定小额奖励（真实后端应从链上查询待领取额，归集后归零）。
	return settlement.NewAssetAmount(big.NewInt(1000), 8), nil
}

func (m *MockBackend) Confirmations(_ context.Context, _chain, _txHash string) (int64, error) {
	return 100, nil // 模拟已超额确认
}
