package options

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

// maxOptionAmount 期权金额（权利金/保证金/行权价/张数/数量）的人类单位上限，
// 用于防止 float 溢出被 AssetAmountFromFloat 静默归零（F5：Inf → 0 导致免费开仓/无抵押空头）。
const maxOptionAmount = 1e15

// finitePositive 校验浮点值为有限正数且不超过上限（F5 边界校验，同 margin.finitePositive）。
func finitePositive(x, max float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0) && x > 0 && x <= max
}

// Config 是期权业务参数。
type Config struct {
	QuoteAsset    string        // 计价资产（默认 USDT）
	RiskFreeRate  float64       // 无风险利率（年化，定价用）
	Volatility    float64       // 波动率（年化，定价用）
	MarginRatio   float64       // 卖方保证金率（按名义价值）
	SettleInterval time.Duration // 后台到期结算轮询间隔
}

// PriceFunc 取标的现货标记价（以 QuoteAsset 计价）。返回 (price, ok)。
type PriceFunc func(underlying string) (float64, bool)

// Service 是期权业务逻辑层，仅依赖 Store 接口与 ledger，不直接拼 SQL。
type Service struct {
	store   Store
	ledger  *ledger.Ledger
	cfg     Config
	log     *zap.Logger
	priceFn PriceFunc

	mu   sync.Mutex
	stop chan struct{}
}

// NewService 构造期权服务。
func NewService(store Store, ledgerSvc *ledger.Ledger, cfg Config, log *zap.Logger, priceFn PriceFunc) *Service {
	if cfg.QuoteAsset == "" {
		cfg.QuoteAsset = "USDT"
	}
	if cfg.RiskFreeRate <= 0 {
		cfg.RiskFreeRate = 0.03
	}
	if cfg.Volatility <= 0 {
		cfg.Volatility = 0.6
	}
	if cfg.MarginRatio <= 0 {
		cfg.MarginRatio = 0.3
	}
	if cfg.SettleInterval <= 0 {
		cfg.SettleInterval = 60 * time.Second
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

// CreateContract 创建期权合约。若未指定 premium，则用 Black-Scholes 基于当前现货价定价。
func (s *Service) CreateContract(c *OptionContract) error {
	if c.Underlying == "" {
		return fmt.Errorf("underlying required")
	}
	if c.Strike <= 0 {
		return fmt.Errorf("strike must be positive")
	}
	if c.Type != TypeCall && c.Type != TypePut {
		return ErrInvalidType
	}
	if c.Style != StyleEuropean && c.Style != StyleAmerican {
		return ErrInvalidStyle
	}
	if c.Expiry.IsZero() {
		return fmt.Errorf("expiry required")
	}
	if c.QuoteAsset == "" {
		c.QuoteAsset = s.cfg.QuoteAsset
	}
	if !settlement.KnownAsset(c.QuoteAsset) {
		return ErrUnsupportedAsset
	}
	// 合约乘数必须为正：负数/零直接拒绝（不再静默置 1，F5-5）。
	if c.ContractSize <= 0 || !finitePositive(c.ContractSize, maxOptionAmount) {
		return fmt.Errorf("contract_size must be positive")
	}
	// 权利金：显式给定优先；否则用 BS 实时定价（需行情）。
	if c.Premium <= 0 {
		spot, ok := s.priceFn(c.Underlying)
		if !ok {
			return ErrPremiumRequired
		}
		t := s.yearsToExpiry(c.Expiry)
		price, _ := BlackScholes(c.Type, spot, c.Strike, t, s.cfg.RiskFreeRate, s.cfg.Volatility)
		c.Premium = price * c.size()
	}
	if math.IsNaN(c.Premium) || math.IsInf(c.Premium, 0) {
		return fmt.Errorf("invalid premium")
	}
	return s.store.CreateContract(c)
}

// yearsToExpiry 返回到期剩余年数（<=0 时为 0）。
func (s *Service) yearsToExpiry(expiry time.Time) float64 {
	d := time.Until(expiry).Hours()
	if d <= 0 {
		return 0
	}
	return d / 8760.0
}

// GetContract 取单条合约。
func (s *Service) GetContract(id int64) (*OptionContract, error) {
	return s.store.GetContract(id)
}

// ListContracts 列出全部合约。
func (s *Service) ListContracts() ([]*OptionContract, error) {
	return s.store.ListContracts()
}

// Quote 用 Black-Scholes 实时计算合约权利金单价（每张）与 Delta。
func (s *Service) Quote(contractID int64) (premium, delta float64, err error) {
	c, err := s.store.GetContract(contractID)
	if err != nil {
		return 0, 0, err
	}
	spot, ok := s.priceFn(c.Underlying)
	if !ok {
		return 0, 0, ErrNoPriceFeed
	}
	t := s.yearsToExpiry(c.Expiry)
	price, d := BlackScholes(c.Type, spot, c.Strike, t, s.cfg.RiskFreeRate, s.cfg.Volatility)
	return price * c.size(), d, nil
}

// OpenPosition 开仓：long 买方支付权利金，short 卖方收权利金并冻结保证金。
//
// 幂等设计（与 otc 一致）：先持久化持仓（StatusOpen），再动账本资金；
// 任一资金动作失败则回滚（删除持仓/解冻），确保「不会出现已扣费却无持仓」，
// 也不会对同一次开仓重复扣费。s.mu 串行化开仓以避免并发重复扣费。
func (s *Service) OpenPosition(userID, contractID int64, side PositionSide, quantity float64) (*OptionPosition, error) {
	if side != SideLong && side != SideShort {
		return nil, ErrInvalidSide
	}
	if !finitePositive(quantity, maxOptionAmount) {
		return nil, ErrInvalidQuantity
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.store.GetContract(contractID)
	if err != nil {
		return nil, err
	}
	if !settlement.KnownAsset(c.QuoteAsset) {
		return nil, ErrUnsupportedAsset
	}
	quote := c.QuoteAsset
	dec := settlement.AssetDecimalsByName(quote)
	premiumTotal := c.Premium * quantity
	if math.IsNaN(premiumTotal) || math.IsInf(premiumTotal, 0) {
		return nil, ErrInvalidQuantity
	}
	premiumAmt := settlement.AssetAmountFromFloat(premiumTotal, dec)

	if side == SideLong {
		avail, _, _ := s.ledger.Balance(userID, quote)
		if avail.Cmp(premiumAmt) < 0 {
			return nil, ErrInsufficientBalance
		}
		// 先落持仓，再扣权利金；扣费失败回滚删除持仓。
		p := &OptionPosition{
			UserID: userID, ContractID: contractID, Side: SideLong, Quantity: quantity,
			QuoteAsset: quote,
			Premium:    settlement.AssetAmountFromFloat(c.Premium, dec),
			Margin:     settlement.AssetAmount{Decimals: dec},
			Status:     StatusOpen,
		}
		if err := s.store.UpsertPosition(p); err != nil {
			return nil, err
		}
		ref := fmt.Sprintf("option_open_long uid=%d cid=%d pos=%d", userID, contractID, p.ID)
		if err := s.ledger.Transfer(userID, ledger.SysOptions, quote, premiumAmt, "option_premium", ref); err != nil {
			_ = s.store.DeletePosition(p.ID)
			return nil, fmt.Errorf("pay premium: %w", err)
		}
		return p, nil
	}

	// short：冻结保证金并收取权利金（来自系统对手方）。
	margin := c.Strike * c.size() * quantity * s.cfg.MarginRatio
	if math.IsNaN(margin) || math.IsInf(margin, 0) {
		return nil, ErrInvalidQuantity
	}
	marginAmt := settlement.AssetAmountFromFloat(margin, dec)
	avail, _, _ := s.ledger.Balance(userID, quote)
	if avail.Cmp(marginAmt) < 0 {
		return nil, ErrInsufficientBalance
	}
	p := &OptionPosition{
		UserID: userID, ContractID: contractID, Side: SideShort, Quantity: quantity,
		QuoteAsset: quote,
		Premium:    settlement.AssetAmountFromFloat(c.Premium, dec),
		Margin:     marginAmt,
		Status:     StatusOpen,
	}
	if err := s.store.UpsertPosition(p); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("option_open_short uid=%d cid=%d pos=%d", userID, contractID, p.ID)
	if err := s.ledger.Freeze(userID, quote, marginAmt); err != nil {
		_ = s.store.DeletePosition(p.ID)
		return nil, fmt.Errorf("freeze margin: %w", err)
	}
	if err := s.ledger.Transfer(ledger.SysOptions, userID, quote, premiumAmt, "option_premium", ref); err != nil {
		_ = s.ledger.Unfreeze(userID, quote, marginAmt)
		_ = s.store.DeletePosition(p.ID)
		return nil, fmt.Errorf("receive premium: %w", err)
	}
	return p, nil
}

// Exercise 由买方（long）主动行权（american 随时；european 需到期）。系统对手方支付内在价值收益。
//
// 幂等设计：先落终态（StatusExercised）再动账本资金；Transfer 失败则回滚状态，
// 重试不会双付。s.mu 串行化以避免并发行权重复吐钱。
func (s *Service) Exercise(userID, positionID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.store.GetPosition(positionID)
	if err != nil {
		return err
	}
	if p.UserID != userID {
		return ErrPositionNotFound
	}
	if p.Status != StatusOpen {
		return ErrAlreadySettled // 终态短路：幂等
	}
	if p.Side != SideLong {
		return ErrNotExercisable // 仅买方可主动行权，卖方在到期结算时处理
	}
	c, err := s.store.GetContract(p.ContractID)
	if err != nil {
		return err
	}
	now := time.Now()
	if c.Style == StyleEuropean && now.Before(c.Expiry) {
		return ErrNotExercisable
	}
	spot, ok := s.priceFn(c.Underlying)
	if !ok || spot <= 0 || math.IsNaN(spot) || math.IsInf(spot, 0) {
		return ErrNoPriceFeed
	}
	dec := settlement.AssetDecimalsByName(c.QuoteAsset)
	itvAmt := settlement.AssetAmountFromFloat(c.IntrinsicValue(spot)*p.Quantity, dec)
	// 中性 CCP：long 开仓时已付权利金给 SysOptions（short 收权利金亦经 SysOptions），
	// 行权时由 SysOptions 支付全部内在价值，不再扣减权利金。原 .Sub(PremiumTotal()) 属
	// 双重计权利金（long 既已付过又从收益里再扣），F3-1 修复。payoff 即内在价值，恒 >=0。
	payoff := itvAmt
	// 先落终态，再动钱。
	p.Status = StatusExercised
	p.UpdatedAt = now
	if err := s.store.UpsertPosition(p); err != nil {
		return err
	}
	if payoff.Sign() > 0 {
		ref := fmt.Sprintf("option_exercise uid=%d pos=%d", userID, positionID)
		if err := s.ledger.Transfer(ledger.SysOptions, userID, c.QuoteAsset, payoff, "option_payoff", ref); err != nil {
			// 回滚：恢复 open 状态，等待重试/对账。
			p.Status = StatusOpen
			p.UpdatedAt = time.Now()
			_ = s.store.UpsertPosition(p)
			return fmt.Errorf("pay payoff: %w", err)
		}
	}
	return nil
}

// SettlePosition 对到期且仍 open 的持仓做结算（long 获收益、short 承担义务）。
// 未到期或价格缺失时安全跳过（返回 settled=false）。
//
// 幂等设计：先落终态（StatusExpired）再动账本资金；资金动作失败则回滚状态，
// 重试不会双付/双退。s.mu 串行化以避免与手动行权/结算并发重复动钱。
func (s *Service) SettlePosition(positionID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.store.GetPosition(positionID)
	if err != nil {
		return false, err
	}
	if p.Status != StatusOpen {
		return false, nil // 终态短路：幂等
	}
	c, err := s.store.GetContract(p.ContractID)
	if err != nil {
		return false, err
	}
	if time.Now().Before(c.Expiry) {
		return false, nil
	}
	spot, ok := s.priceFn(c.Underlying)
	if !ok || spot <= 0 || math.IsNaN(spot) || math.IsInf(spot, 0) {
		return false, nil // 无价格/负价不结算，等下次循环
	}
	quote := c.QuoteAsset
	dec := settlement.AssetDecimalsByName(quote)
	itvAmt := settlement.AssetAmountFromFloat(c.IntrinsicValue(spot)*p.Quantity, dec)

	// 先落终态。
	p.Status = StatusExpired
	p.UpdatedAt = time.Now()
	if err := s.store.UpsertPosition(p); err != nil {
		return false, err
	}

	if p.Side == SideLong {
		// 中性 CCP：long 行权/结算时由 SysOptions 支付全部内在价值（不再扣权利金，F3-1）。
		payoff := itvAmt
		if payoff.Sign() > 0 {
			ref := fmt.Sprintf("option_settle_long pos=%d", positionID)
			if err := s.ledger.Transfer(ledger.SysOptions, p.UserID, quote, payoff, "option_payoff", ref); err != nil {
				// 回滚：恢复 open 状态，等待重试/对账。
				p.Status = StatusOpen
				p.UpdatedAt = time.Now()
				_ = s.store.UpsertPosition(p)
				return false, fmt.Errorf("pay payoff: %w", err)
			}
		}
		return true, nil
	}

	// 卖方：解冻保证金，并在保证金范围内承担内在价值义务（超出由系统已收权利金吸收）。
	if p.Margin.Sign() > 0 {
		_ = s.ledger.Unfreeze(p.UserID, quote, p.Margin)
	}
	payAmt := itvAmt.Min(p.Margin)
	if payAmt.Sign() > 0 {
		ref := fmt.Sprintf("option_settle_short pos=%d", positionID)
		if err := s.ledger.Transfer(p.UserID, ledger.SysOptions, quote, payAmt, "option_loss", ref); err != nil {
			// 回滚：重新冻结保证金并恢复 open 状态。
			if p.Margin.Sign() > 0 {
				_ = s.ledger.Freeze(p.UserID, quote, p.Margin)
			}
			p.Status = StatusOpen
			p.UpdatedAt = time.Now()
			_ = s.store.UpsertPosition(p)
			return false, fmt.Errorf("pay loss: %w", err)
		}
	}
	return true, nil
}

// ListPositions 返回用户全部持仓。
func (s *Service) ListPositions(userID int64) ([]*OptionPosition, error) {
	return s.store.ListPositions(userID)
}

// AdminListPositions 返回全量持仓（管理员视角）。
func (s *Service) AdminListPositions() ([]*OptionPosition, error) {
	return s.store.ListAllPositions()
}

// GetPosition 取单条持仓。
func (s *Service) GetPosition(id int64) (*OptionPosition, error) {
	return s.store.GetPosition(id)
}

// RunLoop 后台循环：周期性对到期且仍 open 的持仓做结算。
func (s *Service) RunLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.SettleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			open, err := s.store.ListAllOpen()
			if err != nil {
				continue
			}
			for _, p := range open {
				if _, serr := s.SettlePosition(p.ID); serr != nil && s.log != nil {
					s.log.Warn("options auto-settle failed",
						zap.Int64("position_id", p.ID), zap.Error(serr))
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
