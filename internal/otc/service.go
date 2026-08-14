package otc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
)

// Config 是 OTC 业务参数。
type Config struct {
	Asset              string        // 默认加密资产（演示用 BTC）
	ReconcileInterval  time.Duration // 后台对账轮询间隔
}

// PriceFunc 取资产参考价（当前 OTC 核心流程不强制依赖行情，预留扩展）。返回 (price, ok)。
type PriceFunc func(asset string) (float64, bool)

// Service 是场外交易业务逻辑层，仅依赖 Store 接口与 ledger，不直接拼 SQL。
type Service struct {
	store   Store
	ledger  *ledger.Ledger
	cfg     Config
	log     *zap.Logger
	priceFn PriceFunc

	mu   sync.Mutex
	stop chan struct{}
}

// NewService 构造 OTC 服务。
func NewService(store Store, ledgerSvc *ledger.Ledger, cfg Config, log *zap.Logger, priceFn PriceFunc) *Service {
	if cfg.Asset == "" {
		cfg.Asset = "BTC"
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = 60 * time.Second
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

// CreateAdvertisement 发布一条 OTC 广告。
func (s *Service) CreateAdvertisement(a *OtcAdvertisement) error {
	if a.Side != SideBuy && a.Side != SideSell {
		return ErrInvalidSide
	}
	if a.Asset == "" {
		a.Asset = s.cfg.Asset
	}
	if a.Price <= 0 {
		return fmt.Errorf("price must be positive")
	}
	if a.MinAmount <= 0 || a.MaxAmount < a.MinAmount {
		return fmt.Errorf("invalid amount range")
	}
	if a.FiatCurrency == "" {
		a.FiatCurrency = "CNY"
	}
	if a.Status == "" {
		a.Status = AdOpen
	}
	return s.store.CreateAd(a)
}

// GetAd 取单条广告。
func (s *Service) GetAd(id int64) (*OtcAdvertisement, error) {
	return s.store.GetAd(id)
}

// ListAdvertisements 浏览开放中的广告（可按方向/资产过滤）。
func (s *Service) ListAdvertisements(side AdSide, asset string) ([]*OtcAdvertisement, error) {
	return s.store.ListAds(side, asset)
}

// TakeOrder 由 taker 吃单，生成订单并冻结卖方 crypto 到中央托管（SysOtc）。
func (s *Service) TakeOrder(adID, takerID int64, fiatAmount float64, paymentMethod string) (*OtcOrder, error) {
	if takerID <= 0 {
		return nil, fmt.Errorf("taker required")
	}
	if fiatAmount <= 0 {
		return nil, ErrInvalidAmount
	}
	ad, err := s.store.GetAd(adID)
	if err != nil {
		return nil, err
	}
	if ad.Status != AdOpen {
		return nil, ErrAdNotOpen
	}
	if ad.UserID == takerID {
		return nil, fmt.Errorf("cannot take own advertisement")
	}
	cryptoAmount := ad.CryptoAmountFor(fiatAmount)
	if cryptoAmount <= 0 {
		return nil, ErrInvalidAmount
	}
	if cryptoAmount < ad.MinAmount || cryptoAmount > ad.MaxAmount {
		return nil, ErrAmountOutOfRange
	}
	seller := takerID
	if ad.Side == SideSell {
		seller = ad.UserID // 卖方广告：maker 是 crypto 卖方
	}
	// 校验卖方可用余额并冻结进托管账户。
	avail, _, _ := s.ledger.Balance(seller, ad.Asset)
	if avail < cryptoAmount-1e-9 {
		return nil, ErrInsufficientBalance
	}
	ref := fmt.Sprintf("otc_take ad=%d seller=%d taker=%d", adID, seller, takerID)
	if err := s.ledger.Transfer(seller, ledger.SysOtc, ad.Asset, cryptoAmount, "otc_escrow", ref); err != nil {
		return nil, fmt.Errorf("lock escrow: %w", err)
	}
	o := &OtcOrder{
		AdID:          adID,
		MakerID:       ad.UserID,
		TakerID:       takerID,
		Side:          ad.Side,
		Asset:         ad.Asset,
		FiatCurrency:  ad.FiatCurrency,
		CryptoAmount:  cryptoAmount,
		Price:         ad.Price,
		FiatAmount:    fiatAmount,
		PaymentMethod: paymentMethod,
		Status:        OrderPending,
	}
	if err := s.store.CreateOrder(o); err != nil {
		// 回滚托管冻结。
		_ = s.ledger.Transfer(ledger.SysOtc, seller, ad.Asset, cryptoAmount, "otc_escrow_rollback", ref)
		return nil, err
	}
	// 标记广告已成交（单笔成交模型，简化）。
	ad.Status = AdFilled
	_ = s.store.UpdateAd(ad)
	return o, nil
}

// MarkPaid 由买方标记法币已支付（状态 pending -> paid）。
func (s *Service) MarkPaid(orderID, userID int64) error {
	o, err := s.store.GetOrder(orderID)
	if err != nil {
		return err
	}
	if o.BuyerID() != userID {
		return ErrNotTaker
	}
	if o.Status != OrderPending {
		return ErrInvalidTransition
	}
	o.Status = OrderPaid
	o.PaidAt = time.Now()
	return s.store.UpdateOrder(o)
}

// ConfirmComplete 由卖方确认收款，crypto 从托管释放给买方（状态 paid -> completed）。
func (s *Service) ConfirmComplete(orderID, userID int64, rating int) error {
	o, err := s.store.GetOrder(orderID)
	if err != nil {
		return err
	}
	if o.SellerID() != userID { // 卖方确认收款
		return ErrNotMaker
	}
	if o.Status != OrderPaid {
		return ErrInvalidTransition
	}
	// 托管释放给买方。
	buyer := o.BuyerID()
	ref := fmt.Sprintf("otc_release ad=%d order=%d", o.AdID, orderID)
	if err := s.ledger.Transfer(ledger.SysOtc, buyer, o.Asset, o.CryptoAmount, "otc_release", ref); err != nil {
		return fmt.Errorf("release escrow: %w", err)
	}
	o.Status = OrderCompleted
	o.CompletedAt = time.Now()
	if rating > 0 && rating <= 5 {
		o.Rating = rating
	}
	if err := s.store.UpdateOrder(o); err != nil {
		return err
	}
	// 更新双向对手方信用。
	s.bumpCounterparty(o.MakerID, o.TakerID, true, rating)
	s.bumpCounterparty(o.TakerID, o.MakerID, true, rating)
	return nil
}

// CancelOrder 由任一方在 pending（crypto 仍在托管、买方未付款）状态下取消，托管退回卖方。
func (s *Service) CancelOrder(orderID, userID int64) error {
	o, err := s.store.GetOrder(orderID)
	if err != nil {
		return err
	}
	if o.MakerID != userID && o.TakerID != userID {
		return ErrNotParty
	}
	if o.Status != OrderPending {
		return ErrInvalidTransition
	}
	return s.returnEscrow(o, OrderCancelled)
}

// OpenDispute 任一方在 paid 后发起争议（状态 paid -> disputed）。
func (s *Service) OpenDispute(orderID, userID int64) error {
	o, err := s.store.GetOrder(orderID)
	if err != nil {
		return err
	}
	if o.MakerID != userID && o.TakerID != userID {
		return ErrNotParty
	}
	if o.Status != OrderPaid {
		return ErrInvalidTransition
	}
	o.Status = OrderDisputed
	return s.store.UpdateOrder(o)
}

// ResolveDispute 管理员裁决争议。refundToSeller=true：托管退回卖方并标记取消；
// refundToSeller=false：托管释放给买方并标记完成（同时计对手方信用）。
func (s *Service) ResolveDispute(orderID int64, refundToSeller bool, rating int) error {
	o, err := s.store.GetOrder(orderID)
	if err != nil {
		return err
	}
	if o.Status != OrderDisputed {
		return ErrInvalidTransition
	}
	if refundToSeller {
		return s.returnEscrow(o, OrderCancelled)
	}
	buyer := o.BuyerID()
	ref := fmt.Sprintf("otc_dispute_release ad=%d order=%d", o.AdID, orderID)
	if err := s.ledger.Transfer(ledger.SysOtc, buyer, o.Asset, o.CryptoAmount, "otc_release", ref); err != nil {
		return fmt.Errorf("release escrow: %w", err)
	}
	o.Status = OrderCompleted
	o.CompletedAt = time.Now()
	if rating > 0 && rating <= 5 {
		o.Rating = rating
	}
	if err := s.store.UpdateOrder(o); err != nil {
		return err
	}
	s.bumpCounterparty(o.MakerID, o.TakerID, true, rating)
	s.bumpCounterparty(o.TakerID, o.MakerID, true, rating)
	return nil
}

// returnEscrow 将托管中的 crypto 退回卖方，并把订单置为终态。
func (s *Service) returnEscrow(o *OtcOrder, final OrderStatus) error {
	seller := o.SellerID()
	ref := fmt.Sprintf("otc_return ad=%d order=%d", o.AdID, o.ID)
	if err := s.ledger.Transfer(ledger.SysOtc, seller, o.Asset, o.CryptoAmount, "otc_return", ref); err != nil {
		return fmt.Errorf("return escrow: %w", err)
	}
	o.Status = final
	return s.store.UpdateOrder(o)
}

// bumpCounterparty 增量更新一对用户的对手方信用（成交次数 / 评分）。
func (s *Service) bumpCounterparty(userID, counterpartyID int64, completed bool, rating int) {
	cp, err := s.store.GetCounterparty(userID, counterpartyID)
	if err == ErrCounterpartyNotFound {
		cp = &OtcCounterparty{UserID: userID, CounterpartyID: counterpartyID}
	} else if err != nil {
		return
	}
	cp.TradesTotal++
	if completed {
		cp.TradesCompleted++
	}
	if rating > 0 && rating <= 5 {
		cp.RatingSum += rating
		cp.RatingCount++
	}
	_ = s.store.UpsertCounterparty(cp)
}

// ListOrders 返回用户参与的订单。
func (s *Service) ListOrders(userID int64) ([]*OtcOrder, error) {
	return s.store.ListOrders(userID)
}

// AdminListOrders 返回全量订单。
func (s *Service) AdminListOrders() ([]*OtcOrder, error) {
	return s.store.ListAllOrders()
}

// GetCounterparty 取单条对手方信用。
func (s *Service) GetCounterparty(userID, counterpartyID int64) (*OtcCounterparty, error) {
	return s.store.GetCounterparty(userID, counterpartyID)
}

// ListCounterparties 列出某用户的全部对手方信用。
func (s *Service) ListCounterparties(userID int64) ([]*OtcCounterparty, error) {
	return s.store.ListCounterparties(userID)
}

// Reconcile 对账：返回中央托管账户余额（应恒为 0）与仍未释放托管的订单（pending/paid/disputed）。
func (s *Service) Reconcile() (escrow float64, stuck []*OtcOrder, err error) {
	escrow, _, _ = s.ledger.Balance(ledger.SysOtc, s.cfg.Asset)
	all, err := s.store.ListAllOrders()
	if err != nil {
		return escrow, nil, err
	}
	for _, o := range all {
		if !o.IsFinal() && o.Status != OrderDisputed {
			// pending / paid 均未释放托管
			cp := *o
			stuck = append(stuck, &cp)
		}
	}
	return escrow, stuck, nil
}

// RunLoop 后台对账循环：周期性检查托管余额，非零或存在未释放订单时告警。
func (s *Service) RunLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			escrow, stuck, err := s.Reconcile()
			if err != nil {
				continue
			}
			if escrow > 1e-9 || len(stuck) > 0 {
				if s.log != nil {
					s.log.Warn("otc reconciliation anomaly",
						zap.Float64("escrow", escrow), zap.Int("stuck_orders", len(stuck)))
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
