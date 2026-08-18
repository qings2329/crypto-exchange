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

// Subscribe 用户委托质押：校验 -> 落委托(占唯一ID) -> 账本锁定本金(SysStaking) -> 链上质押广播。
// F1 幂等：本金锁定的 ref 锚定到具体委托 ID（stake_lock:<id>），与委托一一对应；
//   解锁/重质押均生成新委托与新 ref，不会被账本历史指纹误去重而漏锁（早期版本用
//   stake:<user>:<product> 作 ref，用户解质押后再质押同产品时 ref 已持久化，导致新委托
//   本金未被锁定、账本少记本金——已修复）。重试同一委托因 ref 已落库而安全跳过。
// F2 定点：金额全 AssetAmount；F5 校验：正数/起质押额/资产白名单；F3 原子：任一环节失败
//   均回退（方向相反、biz 不同，指纹不同，不被去重误吞）并删除半成品委托。
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

	// 1) 先落委托占得稳定唯一 ID，作为后续锁本金的幂等锚点。
	d := &StakingDelegation{
		UserID:    userID,
		ProductID: productID,
		Principal:  amount,
		Status:     DelegationActive,
		CreatedAt:  time.Now().Unix(),
	}
	if err := s.store.CreateDelegation(d); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("stake_lock:%d", d.ID)
	// 2) 账本锁定本金（ref 唯一，幂等安全）。
	if err := s.ledger.Transfer(userID, ledger.SysStaking, p.Asset, amount, "stake_lock", ref); err != nil {
		_ = s.store.DeleteDelegation(d.ID)
		return nil, err
	}
	// 3) 链上质押广播。
	txHash, err := s.backend.Stake(context.Background(), p.Chain, p.ContractAddr,
		fmt.Sprintf("%d", userID), p.Validator, amount)
	if err != nil {
		// 回退锁定的本金（方向相反、biz 不同，指纹不同，不被去重）。
		_ = s.ledger.Transfer(ledger.SysStaking, userID, p.Asset, amount, "stake_lock_revert", ref)
		_ = s.store.DeleteDelegation(d.ID)
		return nil, fmt.Errorf("stake on chain: %w", err)
	}
	d.TxHash = txHash
	if err := s.store.UpdateDelegation(d); err != nil {
		// 委托落库失败：回退本金并删除半成品委托。
		_ = s.ledger.Transfer(ledger.SysStaking, userID, p.Asset, amount, "stake_lock_revert", ref)
		_ = s.store.DeleteDelegation(d.ID)
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

// Reconcile 业务对账（F3）：校验「在押委托本金之和 == SysStaking 余额」且
// 「已归集奖励之和 == SysStakingReward 余额」，返回各资产偏差（0 表示平衡）。
// 偏差非 0 意味着业务状态与账本不一致，应触发告警排查。与全局账本复式平衡探针互补：
// 全局探针只保证借贷和为 0，本方法进一步保证业务托管/负债与账本账户逐笔对平。
func (s *Service) Reconcile() map[string]settlement.AssetAmount {
	dev := make(map[string]settlement.AssetAmount)
	delegs, err := s.store.ListAllDelegations()
	if err != nil {
		return dev
	}
	wantStaking := make(map[string]settlement.AssetAmount)
	wantReward := make(map[string]settlement.AssetAmount)
	for _, d := range delegs {
		p, perr := s.store.GetProduct(d.ProductID)
		if perr != nil {
			continue
		}
		// 仅未释放（active/unbonding）委托仍在账本占用本金/累积未付奖励负债；
		// 已释放(unbonded)委托的本金与奖励已在 Release 时发还用户，不再计入应负债。
		if d.Status == DelegationActive || d.Status == DelegationUnbonding {
			wantStaking[p.Asset] = wantStaking[p.Asset].Add(d.Principal)
			rews, _ := s.store.ListRewardsByDelegation(d.ID)
			for _, r := range rews {
				wantReward[p.Asset] = wantReward[p.Asset].Add(r.Amount)
			}
		}
	}
	for asset, want := range wantStaking {
		av, fr, _ := s.ledger.Balance(ledger.SysStaking, asset)
		got := av.Add(fr)
		dev[asset] = dev[asset].Add(got.Sub(want))
	}
	for asset, want := range wantReward {
		av, fr, _ := s.ledger.Balance(ledger.SysStakingReward, asset)
		got := av.Add(fr)
		dev[asset] = dev[asset].Add(got.Sub(want))
	}
	return dev
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
