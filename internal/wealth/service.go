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
//
// 幂等设计（与 otc/options/margin 一致）：先落持仓（HoldingFunding 瞬态）再转入托管本金，
// 转入成功后再置为 HoldingActive；转入失败回滚（删除持仓）。s.mu 串行化申购以避免并发双扣。
// 此前本金转入先于持仓落库、且无锁，崩溃/重试会双扣本金。
func (s *Service) Subscribe(userID, productID int64, amount float64) (*WealthHolding, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user required")
	}
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

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
	// 校验用户可用余额。
	avail, _, _ := s.ledger.Balance(userID, p.Asset)
	if avail.Cmp(settlement.AssetAmountFromFloat(amount, settlement.AssetDecimalsByName(p.Asset))) < 0 {
		return nil, ErrInsufficientBal
	}
	// 先落瞬态持仓，再转入本金。
	h := &WealthHolding{
		UserID:    userID,
		ProductID: productID,
		Principal: amount,
		Status:    HoldingFunding,
	}
	if err := s.store.CreateHolding(h); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("wealth_subscribe product=%d user=%d", productID, userID)
	if err := s.ledger.Transfer(userID, ledger.SysWealth, p.Asset, settlement.AssetAmountFromFloat(amount, settlement.AssetDecimalsByName(p.Asset)), "wealth_subscribe", ref); err != nil {
		_ = s.store.DeleteHolding(h.ID) // 回滚：删掉未出资持仓
		return nil, fmt.Errorf("lock principal: %w", err)
	}
	// 出资成功，置为持有中。
	h.Status = HoldingActive
	if err := s.store.UpdateHolding(h); err != nil {
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
//
// s.mu 串行化：与 Redeem 互斥，避免后台计息与赎回并发导致 AccruedYield 重复累加、赎回时超额兑付。
func (s *Service) Accrue(now time.Time) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
//
// 幂等设计（与 otc/options/margin 一致）：先落终态（HoldingRedeemed）再 Transfer，Transfer 失败回滚状态；
// 顶部终态短路实现幂等（重复赎回不再双付）。s.mu 串行化赎回以避免并发双付、并与后台 Accrue 互斥。
func (s *Service) Redeem(userID, holdingID int64) (*WealthHolding, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	h, err := s.store.GetHolding(holdingID)
	if err != nil {
		return nil, err
	}
	if h.UserID != userID {
		return nil, ErrNotOwner
	}
	if h.Status != HoldingActive {
		return nil, ErrAlreadyRedeemed // 终态短路：幂等
	}
	p, err := s.store.GetProduct(h.ProductID)
	if err != nil {
		return nil, err
	}
	// 定期产品到期前锁定（须在动钱前校验）。
	if p.Type == TypeFixed && p.DurationDays > 0 {
		if time.Now().Before(h.CreatedAt.AddDate(0, 0, p.DurationDays)) {
			return nil, ErrLocked
		}
	}
	now := time.Now()
	_ = s.accrueHolding(h, p, now) // 赎回前补齐到当前的收益
	total := h.Principal + h.AccruedYield
	ref := fmt.Sprintf("wealth_redeem product=%d holding=%d user=%d", h.ProductID, holdingID, userID)

	// 先落终态，再出金。
	h.Status = HoldingRedeemed
	h.RedeemedAt = now
	if err := s.store.UpdateHolding(h); err != nil {
		return nil, err
	}
	if err := s.ledger.Transfer(ledger.SysWealth, userID, p.Asset, settlement.AssetAmountFromFloat(total, settlement.AssetDecimalsByName(p.Asset)), "wealth_redeem", ref); err != nil {
		// 回滚：恢复持有中状态（避免双付）。
		h.Status = HoldingActive
		h.RedeemedAt = time.Time{}
		_ = s.store.UpdateHolding(h)
		return nil, fmt.Errorf("redeem payout: %w", err)
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
