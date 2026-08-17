package margin

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// maxAssetAmount 单笔借入/还款金额的人类单位上限，用于防止 float 溢出被
// AssetAmountFromFloat 静默归零（F5：NaN/Inf → 0 会导致免费借入/无抵押空头）。
const maxAssetAmount = 1e15

// finitePositive 校验浮点值为有限正数且不超过上限。
// 用于边界校验：settlement.AssetAmountFromFloat 对 NaN/Inf 会静默返回 0，
// 须在转换前显式拒绝，否则会产生「免费借入」「零抵押开仓」等资金漏洞（F5）。
func finitePositive(x, max float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0) && x > 0 && x <= max
}

// Config 是杠杆业务参数。
type Config struct {
	MaxLeverage      int           // 最大杠杆倍数
	HourlyRate       float64       // 小时利率（如 0.0001 = 0.01%/h）
	MaintenanceRatio float64       // 维持维持率（强平价 = 抵押 /(债务*维持率)）
	CollateralAsset  string        // 抵押资产（默认 USDT）
	AccrueInterval   time.Duration // 后台计息/强平轮询间隔
	LiquidationPenalty float64     // 强平时抵押罚没比例（如 0.05）
}

// PriceFunc 取某借入资产的标记价（以抵押资产计价，如 USDT）。返回 (price, ok)。
type PriceFunc func(asset string) (float64, bool)

// Service 是现货杠杆业务逻辑层，仅依赖 Store 接口与 ledger，不直接拼 SQL。
type Service struct {
	store   Store
	ledger  *ledger.Ledger
	cfg     Config
	log     *zap.Logger
	priceFn PriceFunc

	mu   sync.Mutex
	stop chan struct{}
}

// NewService 构造杠杆服务。priceFn 可注入（默认 nil 时强平跳过价格评估）。
func NewService(store Store, ledgerSvc *ledger.Ledger, cfg Config, log *zap.Logger, priceFn PriceFunc) *Service {
	if cfg.CollateralAsset == "" {
		cfg.CollateralAsset = "USDT"
	}
	if cfg.AccrueInterval <= 0 {
		cfg.AccrueInterval = 30 * time.Second
	}
	if cfg.LiquidationPenalty <= 0 {
		cfg.LiquidationPenalty = 0.05
	}
	if cfg.MaintenanceRatio <= 0 {
		cfg.MaintenanceRatio = 1.05
	}
	return &Service{
		store:   store,
		ledger:  ledgerSvc,
		cfg:     cfg,
		log:     log,
		priceFn: priceFn,
		stop:    make(chan struct{}),
	}
}

// Borrow 借入 asset 数量 amount，杠杆 leverage（抵押 = amount/leverage 的抵押资产）。
//
// 幂等设计（与 otc/options 一致）：先落账户（StatusActive）再动账本（冻结抵押 + 贷出资产），
// 任一步失败回滚（删除账户/解冻）；同一 (user, asset) 已存在活跃账户时拒绝重复开仓，避免覆盖式双借。
// s.mu 串行化开仓以避免并发重复扣费。
func (s *Service) Borrow(userID int64, asset string, amount float64, leverage int) (*MarginAccount, error) {
	if !settlement.KnownAsset(asset) {
		return nil, ErrUnsupportedAsset
	}
	if !settlement.KnownAsset(s.cfg.CollateralAsset) {
		return nil, ErrUnsupportedAsset
	}
	if !finitePositive(amount, maxAssetAmount) {
		return nil, ErrAmountMustBePositive
	}
	if leverage < 1 {
		return nil, ErrInvalidLeverage
	}
	if leverage > s.cfg.MaxLeverage {
		return nil, ErrOverMaxLeverage
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 已存在活跃账户则拒绝重复开仓（否则 UpsertAccount 会覆盖，造成双借双冻）。
	if existing, err := s.store.GetAccount(userID, asset); err == nil && existing.Status == StatusActive {
		return nil, ErrAlreadyBorrowed
	}

	collateral := amount / float64(leverage)
	collAmt := settlement.AssetAmountFromFloat(collateral, settlement.AssetDecimalsByName(s.cfg.CollateralAsset))
	amt := settlement.AssetAmountFromFloat(amount, settlement.AssetDecimalsByName(asset))

	// 检查抵押是否充足。
	avail, _, ok := s.ledger.Balance(userID, s.cfg.CollateralAsset)
	if !ok || avail.Cmp(collAmt) < 0 {
		return nil, ErrInsufficientCollateral
	}

	// 先落账户，再动账本。
	a := &MarginAccount{
		UserID:           userID,
		Asset:            asset,
		CollateralAsset:  s.cfg.CollateralAsset,
		CollateralAmount: collAmt,
		Debt:             amt,
		InterestAccrued:  settlement.AssetAmount{Decimals: settlement.AssetDecimalsByName(asset)},
		Leverage:         leverage,
		Status:           StatusActive,
		LastAccrual:      time.Now(),
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.store.UpsertAccount(a); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("margin_borrow uid=%d asset=%s", userID, asset)
	if err := s.ledger.Freeze(userID, s.cfg.CollateralAsset, collAmt); err != nil {
		_ = s.store.DeleteAccount(userID, asset)
		return nil, fmt.Errorf("freeze collateral: %w", err)
	}
	if err := s.ledger.CreditAvailable(userID, asset, amt, "margin_borrow", ref); err != nil {
		_ = s.ledger.Unfreeze(userID, s.cfg.CollateralAsset, collAmt)
		_ = s.store.DeleteAccount(userID, asset)
		return nil, fmt.Errorf("credit borrowed asset: %w", err)
	}
	return a, nil
}

// Repay 偿还 asset 数量 amount（先冲本金后冲利息）；还清则解冻抵押并关闭账户。
//
// 幂等设计：先落更新后的账户状态再 Debit，Debit 失败回滚账户；s.mu 串行化避免并发/重试双还。
func (s *Service) Repay(userID int64, asset string, amount float64) error {
	if !settlement.KnownAsset(asset) {
		return ErrUnsupportedAsset
	}
	if !finitePositive(amount, maxAssetAmount) {
		return ErrAmountMustBePositive
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	a, err := s.store.GetAccount(userID, asset)
	if err != nil {
		return err
	}
	if a.Status != StatusActive {
		return ErrAccountLiquidated
	}
	s.accrue(a)
	total := a.TotalOwed()
	if total.IsZero() {
		return ErrNothingOwed
	}
	dec := settlement.AssetDecimalsByName(asset)
	repayAmt := settlement.AssetAmountFromFloat(amount, dec)
	if repayAmt.Cmp(total) > 0 {
		repayAmt = total
	}
	avail, _, ok := s.ledger.Balance(userID, asset)
	if !ok || avail.Cmp(repayAmt) < 0 {
		return ErrInsufficientBalance
	}

	// 计算新状态（定点，去除 1e-9 浮点容差）。
	principalAmt := repayAmt.Min(a.Debt)
	interestPortion := repayAmt.Sub(principalAmt)
	newDebt := a.Debt.Sub(principalAmt)
	newInterest := a.InterestAccrued.Sub(interestPortion)
	if newInterest.Sign() < 0 {
		newInterest = settlement.AssetAmount{Decimals: dec}
	}
	closing := newDebt.IsZero() && newInterest.IsZero()

	// 保存旧快照用于回滚，先落新状态。
	prev := *a
	a.Debt = newDebt
	a.InterestAccrued = newInterest
	if closing {
		a.CollateralAmount = settlement.AssetAmount{Decimals: settlement.AssetDecimalsByName(a.CollateralAsset)}
		a.Status = StatusClosed
	}
	a.UpdatedAt = time.Now()
	if err := s.store.UpsertAccount(a); err != nil {
		return err
	}

	// 动钱：拆分本金与利息，保持复式记账借贷恒等（F3-1）。
	// 本金：burn 用户可用（回补借入时 CreditAvailable 铸造的负债），无对应贷方。
	// 利息：从用户可用划转到平台利息账户 SysMarginInterest，而非 burn——
	//       利息并非凭空铸造，burn 会压低货币供应且平台收入未入账。
	ref := fmt.Sprintf("margin_repay uid=%d asset=%s", userID, asset)
	if principalAmt.Sign() > 0 {
		if err := s.ledger.DebitAvailable(userID, asset, principalAmt, "margin_repay_principal", ref); err != nil {
			*a = prev
			_ = s.store.UpsertAccount(a)
			return err
		}
	}
	if interestPortion.Sign() > 0 {
		if err := s.ledger.Transfer(userID, ledger.SysMarginInterest, asset, interestPortion, "margin_repay_interest", ref); err != nil {
			*a = prev
			_ = s.store.UpsertAccount(a)
			return err
		}
	}
	// 还清则解冻抵押。原实现 "_ = Unfreeze" 静默吞错，可能导致抵押永久冻结（F3-1）。
	// 改为显式校验：解冻失败直接返回错误（账户已置终态、债务已清，重试会命中终态短路不会双还）。
	if closing && prev.CollateralAmount.Sign() > 0 {
		if err := s.ledger.Unfreeze(userID, a.CollateralAsset, prev.CollateralAmount); err != nil {
			if s.log != nil {
				s.log.Error("margin repay: unlock collateral failed", zap.Int64("user_id", userID), zap.String("asset", asset), zap.Error(err))
			}
			return err
		}
	}
	return nil
}

// accrue 按小时利率把利息累计到利息字段（定点累加，避免 float 复利漂移）。
func (s *Service) accrue(a *MarginAccount) {
	now := time.Now()
	hours := now.Sub(a.LastAccrual).Hours()
	if hours > 0 && a.Debt.Sign() > 0 {
		dec := settlement.AssetDecimalsByName(a.Asset)
		delta := settlement.AssetAmountFromFloat(a.Debt.HumanFloat()*s.cfg.HourlyRate*hours, dec)
		a.InterestAccrued = a.InterestAccrued.Add(delta)
		a.LastAccrual = now
	} else if hours > 0 {
		a.LastAccrual = now
	}
}

// Accrue 对外暴露的计息（按需调用，便于接口结算）。
func (s *Service) Accrue(userID int64, asset string) error {
	a, err := s.store.GetAccount(userID, asset)
	if err != nil {
		return err
	}
	if a.Status != StatusActive {
		return ErrAccountLiquidated
	}
	s.accrue(a)
	return s.store.UpsertAccount(a)
}

// LiquidationPrice 返回触发强平的标记价（借入资产以抵押资产计价）。
// 当标记价 > 该值时，债务价值相对固定抵押不足维持率，触发强平。
func (s *Service) LiquidationPrice(userID int64, asset string) (float64, error) {
	a, err := s.store.GetAccount(userID, asset)
	if err != nil {
		return 0, err
	}
	if a.Status != StatusActive {
		return 0, ErrAccountLiquidated
	}
	s.accrue(a)
	if a.Debt.IsZero() {
		return 0, nil
	}
	// 抵押价值 /(债务 * 维持率)
	return a.CollateralAmount.HumanFloat() / (a.Debt.HumanFloat() * s.cfg.MaintenanceRatio), nil
}

// Liquidate 评估并在越界时执行强平：收回借出资产、罚没部分抵押入保险基金、剩余解冻。
// 返回是否执行了强平。
//
// 幂等设计：先落终态（StatusLiquidated）再动账本（扣回借出资产/解冻抵押/罚没），
// 任一步失败回滚账户与抵押；顶部终态短路实现幂等（重复调用不再双占双罚）。s.mu 串行化
// 避免与后台 RunLoop 并发强平重复动钱。
func (s *Service) Liquidate(userID int64, asset string) (bool, error) {
	if !settlement.KnownAsset(asset) {
		return false, ErrUnsupportedAsset
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	a, err := s.store.GetAccount(userID, asset)
	if err != nil {
		return false, err
	}
	if a.Status != StatusActive {
		return false, nil // 终态短路：幂等
	}
	s.accrue(a)
	if s.priceFn == nil {
		return false, nil
	}
	price, ok := s.priceFn(asset)
	if !ok {
		return false, nil
	}
	liqPrice, err := s.LiquidationPrice(userID, asset)
	if err != nil {
		return false, err
	}
	if price <= liqPrice {
		return false, nil // 安全，未越界
	}

	// 计算强平动作金额。
	collAmt := a.CollateralAmount
	penaltyAmt := settlement.AssetAmountFromFloat(a.CollateralAmount.HumanFloat()*s.cfg.LiquidationPenalty, settlement.AssetDecimalsByName(a.CollateralAsset))
	avail, _, ok2 := s.ledger.Balance(userID, asset)
	seize := a.Debt
	if !ok2 || avail.Cmp(seize) < 0 {
		seize = avail
	}
	ref := fmt.Sprintf("margin_liq uid=%d asset=%s", userID, asset)

	// 先落终态，再动钱。
	prev := *a
	a.CollateralAmount = settlement.AssetAmount{Decimals: settlement.AssetDecimalsByName(a.CollateralAsset)}
	a.Debt = settlement.AssetAmount{Decimals: settlement.AssetDecimalsByName(a.Asset)}
	a.InterestAccrued = settlement.AssetAmount{Decimals: settlement.AssetDecimalsByName(a.Asset)}
	a.Status = StatusLiquidated
	a.UpdatedAt = time.Now()
	if err := s.store.UpsertAccount(a); err != nil {
		return false, err
	}

	// 清算资金动作（收回借出资产/解冻抵押/罚没入保险基金）经账本 Batch 原子执行：
	// 任一步失败整体回滚（账户与抵押/终态恢复），避免部分解冻/部分罚没导致账实不一致。
	// 此前三步各自调用并手工回滚，存在"第一步成功、第二步失败"留下半成品状态的风险。
	ops := []ledger.Op{}
	if seize.Sign() > 0 {
		ops = append(ops, ledger.Op{Kind: ledger.OpDebit, User: userID, Asset: asset, Amount: seize, Biz: "margin_liquidation", Ref: ref})
	}
	if prev.CollateralAmount.Sign() > 0 {
		ops = append(ops, ledger.Op{Kind: ledger.OpUnfreeze, User: userID, Asset: a.CollateralAsset, Amount: collAmt, Ref: ref})
	}
	if penaltyAmt.Sign() > 0 {
		ops = append(ops, ledger.Op{Kind: ledger.OpTransfer, From: userID, To: ledger.SysInsurance, Asset: a.CollateralAsset, Amount: penaltyAmt, Biz: "margin_liq_penalty", Ref: ref})
	}
	if len(ops) > 0 {
		if err := s.ledger.Batch(ops); err != nil {
			// 整组原子回滚：恢复抵押与终态，等待重试（终态短路保证不双占双罚）。
			*a = prev
			_ = s.store.UpsertAccount(a)
			return false, err
		}
	}
	if s.log != nil {
		s.log.Warn("margin account liquidated",
			zap.Int64("user_id", userID), zap.String("asset", asset),
			zap.Float64("price", price), zap.Float64("liq_price", liqPrice))
	}
	return true, nil
}

// Account 返回单条账户（含最新计息）。
func (s *Service) Account(userID int64, asset string) (*MarginAccount, error) {
	a, err := s.store.GetAccount(userID, asset)
	if err != nil {
		return nil, err
	}
	if a.Status == StatusActive {
		s.accrue(a)
	}
	return a, nil
}

// Accounts 返回用户全部账户。
func (s *Service) Accounts(userID int64) ([]*MarginAccount, error) {
	return s.store.ListAccounts(userID)
}

// RunLoop 后台循环：周期性计息并对所有活跃账户评估强平。
func (s *Service) RunLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.AccrueInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			acts, err := s.store.ListAllActive()
			if err != nil {
				continue
			}
			for _, a := range acts {
				if _, lerr := s.Liquidate(a.UserID, a.Asset); lerr != nil && s.log != nil {
					s.log.Warn("margin auto-liquidate failed",
						zap.Int64("user_id", a.UserID), zap.String("asset", a.Asset), zap.Error(lerr))
				}
			}
		}
	}
}

// Close 停止后台循环。
func (s *Service) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}
