package otc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// Config 是 OTC 业务参数。
type Config struct {
	Asset             string        // 默认加密资产（演示用 BTC）
	ReconcileInterval time.Duration // 后台对账轮询间隔
	UploadDir         string        // 付款凭证文件落盘目录（默认 uploads/otc）
}

// PriceFunc 取资产参考价（当前 OTC 核心流程不强制依赖行情，预留扩展）。返回 (price, ok)。
type PriceFunc func(asset string) (float64, bool)

// Service 是场外交易业务逻辑层，仅依赖 Store 接口与 ledger，不直接拼 SQL。
type Service struct {
	store     Store
	ledger    *ledger.Ledger
	cfg       Config
	log       *zap.Logger
	priceFn   PriceFunc
	uploadDir string

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
	if cfg.UploadDir == "" {
		cfg.UploadDir = "uploads/otc"
	}
	return &Service{
		store:     store,
		ledger:    ledgerSvc,
		cfg:       cfg,
		log:       log,
		priceFn:   priceFn,
		uploadDir: cfg.UploadDir,
		stop:      make(chan struct{}),
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
	dec := settlement.AssetDecimalsByName(ad.Asset)
	cryptoAmount := ad.CryptoAmountFor(fiatAmount, ad.Asset)
	if cryptoAmount.Sign() <= 0 {
		return nil, ErrInvalidAmount
	}
	// M5：ad.MinAmount/MaxAmount 为用户建广告时提交并经落库的金额，须拦截 NaN/Inf，避免区间记 0 致任意成交。
	minA, err := settlement.AssetAmountFromFloatSafe(ad.MinAmount, dec)
	if err != nil {
		return nil, err
	}
	maxA, err := settlement.AssetAmountFromFloatSafe(ad.MaxAmount, dec)
	if err != nil {
		return nil, err
	}
	if cryptoAmount.Cmp(minA) < 0 || cryptoAmount.Cmp(maxA) > 0 {
		return nil, ErrAmountOutOfRange
	}
	seller := takerID
	if ad.Side == SideSell {
		seller = ad.UserID // 卖方广告：maker 是 crypto 卖方
	}
	// 校验卖方可用余额并冻结进托管账户。
	avail, _, _ := s.ledger.Balance(seller, ad.Asset)
	if avail.Cmp(cryptoAmount) < 0 {
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
	// 幂等释放：终态短路 + 同一笔托管只释放一次（见 settle）。
	return s.settle(o, OrderCompleted, true, rating, []OrderStatus{OrderPaid})
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

// ResolveDispute 管理员裁决争议（需 AdminGuard，见 handler）。refundToSeller=true：托管退回卖方并标记取消；
// refundToSeller=false：托管释放给买方并标记完成（同时计对手方信用）。幂等释放，重复裁决不会双付/双退。
func (s *Service) ResolveDispute(orderID int64, refundToSeller bool, rating int) error {
	o, err := s.store.GetOrder(orderID)
	if err != nil {
		return err
	}
	if o.IsFinal() {
		return nil // 幂等：已裁决为终态，视为成功（不重复移动资金）
	}
	if o.Status != OrderDisputed {
		return ErrInvalidTransition
	}
	if refundToSeller {
		return s.settle(o, OrderCancelled, false, rating, []OrderStatus{OrderDisputed})
	}
	return s.settle(o, OrderCompleted, true, rating, []OrderStatus{OrderDisputed})
}

// returnEscrow 将托管中的 crypto 退回卖方，并把订单置为终态（仅用于 pending 取消，幂等）。
func (s *Service) returnEscrow(o *OtcOrder, final OrderStatus) error {
	return s.settle(o, final, false, 0, []OrderStatus{OrderPending})
}

// settle 以幂等方式把托管中的 crypto 释放/退回给买方或卖方，并把订单置为终态。
//
// 防双付/双退（F1）：早期实现是「先 ledger.Transfer 后 UpdateOrder」，若 Transfer 成功但
// UpdateOrder 失败/进程崩溃/重试，订单仍处非终态，再次调用会重复转账。本实现改为：
//  1. 在 s.mu 下重新读取订单，若已是终态则直接成功返回（终态短路，防并发/重试重入）；
//  2. 先把订单置为终态并落库（原子「已结算」标记），再执行 ledger.Transfer；
//  3. 若 Transfer 失败，回滚状态为原值以便安全重试；仅当 Transfer 成功才真正终态化。
//
// 由此：重试/崩溃后不会重复转账（终态已落库则短路；未落库则旧状态允许重试）。
// 残余边界（Transfer 成功但落库前进程崩溃）会使订单为终态而托管未释放——资金安全（不双付），
// 仅托管暂挂，由后台对账（RunLoop）告警并需运营介入重释放，已在注释中标注。
func (s *Service) settle(o *OtcOrder, final OrderStatus, toBuyer bool, rating int, allowedPre []OrderStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.store.GetOrder(o.ID)
	if err != nil {
		return err
	}
	if cur.IsFinal() {
		return nil // 幂等：已终态，视为成功（不重复转账）
	}
	valid := false
	for _, p := range allowedPre {
		if cur.Status == p {
			valid = true
			break
		}
	}
	if !valid {
		return ErrInvalidTransition
	}
	prev := cur.Status
	cur.Status = final
	if final == OrderCompleted {
		cur.CompletedAt = time.Now()
	}
	if rating > 0 && rating <= 5 {
		cur.Rating = rating
	}
	// 先落库终态标记，作为「已结算」的持久化断言。
	if err := s.store.UpdateOrder(cur); err != nil {
		return err
	}
	dest := cur.SellerID()
	if toBuyer {
		dest = cur.BuyerID()
	}
	ref := fmt.Sprintf("otc_settle ad=%d order=%d", cur.AdID, cur.ID)
	if err := s.ledger.Transfer(ledger.SysOtc, dest, cur.Asset, cur.CryptoAmount, "otc_release", ref); err != nil {
		// 转账失败：回滚状态以便安全重试（不会留下终态导致重复释放）。
		cur.Status = prev
		_ = s.store.UpdateOrder(cur)
		return fmt.Errorf("settle escrow: %w", err)
	}
	if final == OrderCompleted {
		s.bumpCounterparty(cur.MakerID, cur.TakerID, true, rating)
		s.bumpCounterparty(cur.TakerID, cur.MakerID, true, rating)
	}
	return nil
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

// --- 订单沟通 / 付款凭证 ---

// sendOrReadGuard 复用：消息/凭证都要求调用者是订单参与方（maker 或 taker）。
func (s *Service) orderPartyGuard(orderID, userID int64) (*OtcOrder, error) {
	o, err := s.store.GetOrder(orderID)
	if err != nil {
		return nil, err
	}
	if o.MakerID != userID && o.TakerID != userID {
		return nil, ErrNotParty
	}
	return o, nil
}

// SendMessage 由订单一方发送一条沟通消息（落库，不限订单状态）。
func (s *Service) SendMessage(orderID, senderID int64, content string) (*OtcMessage, error) {
	if _, err := s.orderPartyGuard(orderID, senderID); err != nil {
		return nil, err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("message content required")
	}
	if len([]rune(content)) > 2000 {
		return nil, fmt.Errorf("message too long")
	}
	m := &OtcMessage{OrderID: orderID, SenderID: senderID, Content: content, CreatedAt: time.Now()}
	if err := s.store.CreateMessage(m); err != nil {
		return nil, err
	}
	return m, nil
}

// ListMessages 列出订单全部沟通消息（按时间升序）。
func (s *Service) ListMessages(orderID, userID int64) ([]*OtcMessage, error) {
	if _, err := s.orderPartyGuard(orderID, userID); err != nil {
		return nil, err
	}
	return s.store.ListMessages(orderID)
}

// UploadProof 由订单一方上传付款凭证：文件落本地磁盘，仅持久化元数据与可访问 URL。
func (s *Service) UploadProof(orderID, uploaderID int64, fileName, contentType string, data []byte) (*OtcProof, error) {
	if _, err := s.orderPartyGuard(orderID, uploaderID); err != nil {
		return nil, err
	}
	if fileName == "" || len(data) == 0 {
		return nil, fmt.Errorf("proof file required")
	}
	if len(data) > 10<<20 {
		return nil, fmt.Errorf("proof too large (max 10MB)")
	}
	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir upload dir: %w", err)
	}
	// 文件名安全化：保留原扩展名（截断到 8 字符以内），主体用订单号+纳秒时间戳，避免覆盖/穿越。
	ext := filepath.Ext(fileName)
	if len(ext) > 8 {
		ext = ""
	}
	stored := fmt.Sprintf("%d_%d%s", orderID, time.Now().UnixNano(), ext)
	full := filepath.Join(s.uploadDir, stored)
	if err := os.WriteFile(full, data, 0o600); err != nil {
		return nil, fmt.Errorf("write proof: %w", err)
	}
	p := &OtcProof{
		OrderID:     orderID,
		UploaderID:  uploaderID,
		FileName:    fileName,
		ContentType: contentType,
		Size:        int64(len(data)),
		URL:         "/api/v1/otc/orders/" + itoa(orderID) + "/proofs/" + stored,
		CreatedAt:   time.Now(),
	}
	if err := s.store.CreateProof(p); err != nil {
		_ = os.Remove(full)
		return nil, err
	}
	return p, nil
}

// ListProofs 列出订单全部付款凭证元数据。
func (s *Service) ListProofs(orderID, userID int64) ([]*OtcProof, error) {
	if _, err := s.orderPartyGuard(orderID, userID); err != nil {
		return nil, err
	}
	return s.store.ListProofs(orderID)
}

// Reconcile 对账：按资产分别返回中央托管账户余额（应恒为 0）与仍未释放托管的订单（pending/paid/disputed）。
// F3：按订单实际资产分别 Balance(SysOtc, asset)，而非仅默认 cfg.Asset（多资产时不漏检 misbucket）。
// F5b：余额以 settlement.AssetAmount 整数表示，比较用 IsZero，不引入 1e-9 浮点容差。
func (s *Service) Reconcile() (escrow map[string]settlement.AssetAmount, stuck []*OtcOrder, err error) {
	all, err := s.store.ListAllOrders()
	if err != nil {
		return nil, nil, err
	}
	// 默认资产 + 所有订单实际出现的资产，逐一核对托管余额。
	assets := map[string]struct{}{s.cfg.Asset: {}}
	for _, o := range all {
		if o.Asset != "" {
			assets[o.Asset] = struct{}{}
		}
	}
	escrow = make(map[string]settlement.AssetAmount, len(assets))
	for a := range assets {
		bal, _, _ := s.ledger.Balance(ledger.SysOtc, a)
		escrow[a] = bal
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
			escrows, stuck, err := s.Reconcile()
			if err != nil {
				continue
			}
			nonZeroAssets := 0
			for _, bal := range escrows {
				if !bal.IsZero() {
					nonZeroAssets++
				}
			}
			if nonZeroAssets > 0 || len(stuck) > 0 {
				if s.log != nil {
					s.log.Warn("otc reconciliation anomaly",
						zap.Int("nonzero_escrow_assets", nonZeroAssets),
						zap.Int("stuck_orders", len(stuck)))
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
