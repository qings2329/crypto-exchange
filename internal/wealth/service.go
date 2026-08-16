package wealth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// Config 是理财资管业务参数。
type Config struct {
	AccrueInterval time.Duration // 后台应计收益轮询间隔
}

// Service 是理财资管业务逻辑层，仅依赖 Store 接口与 ledger，不直接拼 SQL。
type Service struct {
	store  Store
	ledger *ledger.Ledger
	cfg    Config
	log    *zap.Logger

	mu   sync.Mutex
	stop chan struct{}
}

// NewService 构造理财资管服务。
func NewService(store Store, ledgerSvc *ledger.Ledger, cfg Config, log *zap.Logger) *Service {
	if cfg.AccrueInterval <= 0 {
		cfg.AccrueInterval = 60 * time.Second
	}
	return &Service{
		store:  store,
		ledger: ledgerSvc,
		cfg:    cfg,
		log:    log,
		stop:   make(chan struct{}),
	}
}

// CreateProduct 发行一个理财产品（平台/管理员操作）。
func (s *Service) CreateProduct(p *WealthProduct) error {
	if p.Type != TypeCurrent && p.Type != TypeFixed {
		return ErrInvalidType
	}
	if p.AnnualRate < 0 {
		return ErrInvalidRate
	}
	if p.DurationDays < 0 {
		return ErrInvalidDuration
	}
	if p.Asset == "" {
		p.Asset = "USDT"
	}
	if p.Status == "" {
		p.Status = ProductOpen
	}
	if p.MinAmount <= 0 {
		p.MinAmount = 1
	}
	return s.store.CreateProduct(p)
}

// GetProduct 取单个产品。
func (s *Service) GetProduct(id int64) (*WealthProduct, error) {
	return s.store.GetProduct(id)
}

// ListProducts 列出产品（可按状态过滤，默认在售）。
func (s *Service) ListProducts(status ProductStatus) ([]*WealthProduct, error) {
	if status == "" {
		status = ProductOpen
	}
	return s.store.ListProducts(status)
}

// Subscribe 用户申购：校验产品与起购额，扣减用户可用本金转入 SysWealth 中央托管，生成持仓。
func (s *Service) Subscribe(userID, productID int64, amount float64) (*WealthHolding, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user required")
	}
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	p, err := s.store.GetProduct(productID)
	if err != nil {
		return nil, err
	}
	if p.Status != ProductOpen {
		return nil, ErrProductNotOpen
	}
	if amount < p.MinAmount-1e-9 {
		return nil, ErrBelowMinAmount
	}
	// 校验用户可用余额并转入托管。
	avail, _, _ := s.ledger.Balance(userID, p.Asset)
	if avail.Cmp(settlement.AssetAmountFromFloat(amount, settlement.AssetDecimalsByName(p.Asset))) < 0 {
		return nil, ErrInsufficientBal
	}
	ref := fmt.Sprintf("wealth_subscribe product=%d user=%d", productID, userID)
	if err := s.ledger.Transfer(userID, ledger.SysWealth, p.Asset, settlement.AssetAmountFromFloat(amount, settlement.AssetDecimalsByName(p.Asset)), "wealth_subscribe", ref); err != nil {
		return nil, fmt.Errorf("lock principal: %w", err)
	}
	h := &WealthHolding{
		UserID:    userID,
		ProductID: productID,
		Principal: amount,
		Status:    HoldingActive,
	}
	if err := s.store.CreateHolding(h); err != nil {
		// 回滚托管转入。
		_ = s.ledger.Transfer(ledger.SysWealth, userID, p.Asset, settlement.AssetAmountFromFloat(amount, settlement.AssetDecimalsByName(p.Asset)), "wealth_subscribe_rollback", ref)
		return nil, err
	}
	return h, nil
}

// accrueHolding 对单笔持仓按当前时间应计收益（把本金 × 年化 × 小时 计入 AccruedYield）。
// 返回本轮回填的收益额。
func (s *Service) accrueHolding(h *WealthHolding, p *WealthProduct, now time.Time) float64 {
	delta := h.YieldTo(now, p.AnnualRate)
	if delta > 0 {
		h.AccruedYield += delta
		h.LastAccrualAt = now
	}
	return delta
}

// Accrue 对全部持有中持仓执行一次应计收益（通常在后台循环调用）。返回本轮回填的总收益。
func (s *Service) Accrue(now time.Time) (float64, error) {
	all, err := s.store.ListAllHoldings()
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, h := range all {
		if h.Status != HoldingActive {
			continue
		}
		p, err := s.store.GetProduct(h.ProductID)
		if err != nil {
			continue
		}
		if d := s.accrueHolding(h, p, now); d > 0 {
			total += d
			_ = s.store.UpdateHolding(h)
		}
	}
	return total, nil
}

// Redeem 用户赎回持仓：活期随时可赎；定期须到期。本金+应计收益从 SysWealth 支出给用户。
func (s *Service) Redeem(userID, holdingID int64) (*WealthHolding, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user required")
	}
	h, err := s.store.GetHolding(holdingID)
	if err != nil {
		return nil, err
	}
	if h.UserID != userID {
		return nil, ErrNotOwner
	}
	if h.Status != HoldingActive {
		return nil, ErrAlreadyRedeemed
	}
	p, err := s.store.GetProduct(h.ProductID)
	if err != nil {
		return nil, err
	}
	// 定期产品到期前锁定。
	if p.Type == TypeFixed && p.DurationDays > 0 {
		if time.Now().Before(h.CreatedAt.AddDate(0, 0, p.DurationDays)) {
			return nil, ErrLocked
		}
	}
	now := time.Now()
	_ = s.accrueHolding(h, p, now) // 赎回前补齐到当前的收益
	total := h.Principal + h.AccruedYield
	ref := fmt.Sprintf("wealth_redeem product=%d holding=%d user=%d", h.ProductID, holdingID, userID)
	if err := s.ledger.Transfer(ledger.SysWealth, userID, p.Asset, settlement.AssetAmountFromFloat(total, settlement.AssetDecimalsByName(p.Asset)), "wealth_redeem", ref); err != nil {
		return nil, fmt.Errorf("redeem payout: %w", err)
	}
	h.Status = HoldingRedeemed
	h.RedeemedAt = now
	if err := s.store.UpdateHolding(h); err != nil {
		// 回滚支出（极端情况：store 失败但账本已出金）。
		_ = s.ledger.Transfer(userID, ledger.SysWealth, p.Asset, settlement.AssetAmountFromFloat(total, settlement.AssetDecimalsByName(p.Asset)), "wealth_redeem_rollback", ref)
		return nil, err
	}
	return h, nil
}

// MyHoldings 返回某用户的持仓（含应计收益的最新视图）。
func (s *Service) MyHoldings(userID int64) ([]*WealthHolding, error) {
	return s.store.ListHoldings(userID)
}

// AdminListHoldings 返回全量持仓。
func (s *Service) AdminListHoldings() ([]*WealthHolding, error) {
	return s.store.ListAllHoldings()
}

// RunLoop 后台应计收益循环。
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
			if _, err := s.Accrue(time.Now()); err != nil && s.log != nil {
				s.log.Warn("wealth accrue failed", zap.Error(err))
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
