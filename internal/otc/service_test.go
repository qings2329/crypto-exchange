package otc

import (
	"testing"

	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// eqAmt 将账本返回的 AssetAmount 与人类单位字面量按资产小数位精确比较（无 epsilon）。
func eqAmt(a settlement.AssetAmount, human float64, asset string) bool {
	return a.Cmp(settlement.AssetAmountFromFloat(human, settlement.AssetDecimalsByName(asset))) == 0
}

func newTestService() (*Service, *ledger.Ledger) {
	store := NewMemStore()
	l := ledger.New()
	for _, uid := range []int64{1, 2} {
		_ = l.Deposit(uid, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), "seed")
		_ = l.Deposit(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed")
	}
	svc := NewService(store, l, Config{}, zap.NewNop(), func(string) (float64, bool) { return 0, false })
	return svc, l
}

// 发布一条卖方广告（maker=1 卖出 BTC）。
func mustSellAd(svc *Service) *OtcAdvertisement {
	ad := &OtcAdvertisement{
		UserID: 1, Side: SideSell, Asset: "BTC", FiatCurrency: "CNY",
		Price: 60000, MinAmount: 0.1, MaxAmount: 5, PaymentMethods: "bank",
	}
	if err := svc.CreateAdvertisement(ad); err != nil {
		panic(err)
	}
	return ad
}

func TestCreateAndListAd(t *testing.T) {
	svc, _ := newTestService()
	_ = mustSellAd(svc)
	if err := svc.CreateAdvertisement(&OtcAdvertisement{
		UserID: 2, Side: "bad", Asset: "BTC", Price: 1, MinAmount: 1, MaxAmount: 2,
	}); err != ErrInvalidSide {
		t.Fatalf("expected ErrInvalidSide, got %v", err)
	}
	ads, err := svc.ListAdvertisements(SideSell, "BTC")
	if err != nil || len(ads) != 1 {
		t.Fatalf("list ads failed: %v len=%d", err, len(ads))
	}
}

func TestTakeOrderLocksEscrow(t *testing.T) {
	svc, l := newTestService()
	ad := mustSellAd(svc)
	// taker=2 吃 60000 法币 -> 1 BTC；卖方为 maker=1，BTC 冻结入托管。
	o, err := svc.TakeOrder(ad.ID, 2, 60000, "bank")
	if err != nil {
		t.Fatalf("take order: %v", err)
	}
	if o.SellerID() != 1 || o.BuyerID() != 2 || o.CryptoAmount.Cmp(settlement.AssetAmountFromInt64(1, settlement.AssetDecimalsByName("BTC"))) != 0 {
		t.Fatalf("unexpected order: %+v", o)
	}
	avail, _, _ := l.Balance(1, "BTC")
	if !eqAmt(avail, 9, "BTC") {
		t.Fatalf("seller btc not reduced: avail=%v", avail)
	}
	escrow, _, _ := l.Balance(ledger.SysOtc, "BTC")
	if !eqAmt(escrow, 1, "BTC") {
		t.Fatalf("escrow not 1: %v", escrow)
	}
}

func TestTakeOrderRejectsOwnAd(t *testing.T) {
	svc, _ := newTestService()
	ad := mustSellAd(svc)
	if _, err := svc.TakeOrder(ad.ID, 1, 60000, "bank"); err == nil {
		t.Fatal("should reject taking own ad")
	}
}

func TestCompleteFlowReleasesEscrow(t *testing.T) {
	svc, l := newTestService()
	ad := mustSellAd(svc)
	o, err := svc.TakeOrder(ad.ID, 2, 60000, "bank")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	// 买方标记已付款。
	if err := svc.MarkPaid(o.ID, 2); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	// 卖方确认收款并评分。
	if err := svc.ConfirmComplete(o.ID, 1, 5); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// 托管清空，买方获得 1 BTC。
	escrow, _, _ := l.Balance(ledger.SysOtc, "BTC")
	if escrow.Sign() > 0 {
		t.Fatalf("escrow not cleared: %v", escrow)
	}
	buyerAvail, _, _ := l.Balance(2, "BTC")
	if !eqAmt(buyerAvail, 11, "BTC") {
		t.Fatalf("buyer btc wrong: %v", buyerAvail)
	}
	// 对手方信用更新。
	cp, err := svc.GetCounterparty(1, 2)
	if err != nil || cp.TradesTotal != 1 || cp.TradesCompleted != 1 || cp.RatingSum != 5 {
		t.Fatalf("counterparty not updated: err=%v cp=%+v", err, cp)
	}
}

func TestCancelOrderReturnsEscrow(t *testing.T) {
	svc, l := newTestService()
	ad := mustSellAd(svc)
	o, err := svc.TakeOrder(ad.ID, 2, 60000, "bank")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := svc.CancelOrder(o.ID, 2); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	escrow, _, _ := l.Balance(ledger.SysOtc, "BTC")
	if escrow.Sign() > 0 {
		t.Fatalf("escrow not returned: %v", escrow)
	}
	sellerAvail, _, _ := l.Balance(1, "BTC")
	if sellerAvail.Cmp(settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC"))) < 0 {
		t.Fatalf("seller btc not restored: %v", sellerAvail)
	}
}

func TestInsufficientBalanceOnTake(t *testing.T) {
	store := NewMemStore()
	l := ledger.New()
	_ = l.Deposit(1, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), "seed") // maker 有 BTC
	// taker=2 无 BTC；buy 广告：taker 是卖方，需冻结 BTC -> 应失败。
	svc := NewService(store, l, Config{}, zap.NewNop(), func(string) (float64, bool) { return 0, false })
	ad := &OtcAdvertisement{
		UserID: 1, Side: SideBuy, Asset: "BTC", FiatCurrency: "CNY",
		Price: 60000, MinAmount: 0.1, MaxAmount: 5,
	}
	if err := svc.CreateAdvertisement(ad); err != nil {
		t.Fatalf("create ad: %v", err)
	}
	if _, err := svc.TakeOrder(ad.ID, 2, 60000, "bank"); err != ErrInsufficientBalance {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestDisputeResolutionRefundToBuyer(t *testing.T) {
	svc, l := newTestService()
	ad := mustSellAd(svc)
	o, err := svc.TakeOrder(ad.ID, 2, 60000, "bank")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := svc.MarkPaid(o.ID, 2); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if err := svc.OpenDispute(o.ID, 1); err != nil {
		t.Fatalf("dispute: %v", err)
	}
	// 管理员裁决：释放托管给买方。
	if err := svc.ResolveDispute(o.ID, false, 4); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	escrow, _, _ := l.Balance(ledger.SysOtc, "BTC")
	if escrow.Sign() > 0 {
		t.Fatalf("escrow not released: %v", escrow)
	}
	buyerAvail, _, _ := l.Balance(2, "BTC")
	if buyerAvail.Cmp(settlement.AssetAmountFromFloat(11, settlement.AssetDecimalsByName("BTC"))) < 0 {
		t.Fatalf("buyer btc wrong: %v", buyerAvail)
	}
}

// TestConfirmCompleteIdempotent 验证 F1：重复确认收款（模拟重试/崩溃后重放）不会双付/双释放。
func TestConfirmCompleteIdempotent(t *testing.T) {
	svc, l := newTestService()
	ad := mustSellAd(svc)
	o, err := svc.TakeOrder(ad.ID, 2, 60000, "bank")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := svc.MarkPaid(o.ID, 2); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if err := svc.ConfirmComplete(o.ID, 1, 5); err != nil {
		t.Fatalf("complete#1: %v", err)
	}
	// 重放同一笔确认（终态短路，应成功且不重复转账）。
	if err := svc.ConfirmComplete(o.ID, 1, 5); err != nil {
		t.Fatalf("complete#2 (idempotent): %v", err)
	}
	// 买方应恰好获得 1 BTC，托管清零（不得为 2 BTC / 非零托管）。
	buyerAvail, _, _ := l.Balance(2, "BTC")
	if !eqAmt(buyerAvail, 11, "BTC") {
		t.Fatalf("buyer btc double-released: %v", buyerAvail)
	}
	escrow, _, _ := l.Balance(ledger.SysOtc, "BTC")
	if escrow.Sign() != 0 {
		t.Fatalf("escrow not cleared after idempotent complete: %v", escrow)
	}
}

// TestResolveDisputeIdempotent 验证 F1：管理员重复裁决不会双付/双退。
func TestResolveDisputeIdempotent(t *testing.T) {
	svc, l := newTestService()
	ad := mustSellAd(svc)
	o, err := svc.TakeOrder(ad.ID, 2, 60000, "bank")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := svc.MarkPaid(o.ID, 2); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if err := svc.OpenDispute(o.ID, 1); err != nil {
		t.Fatalf("dispute: %v", err)
	}
	if err := svc.ResolveDispute(o.ID, false, 4); err != nil {
		t.Fatalf("resolve#1: %v", err)
	}
	if err := svc.ResolveDispute(o.ID, false, 4); err != nil {
		t.Fatalf("resolve#2 (idempotent): %v", err)
	}
	buyerAvail, _, _ := l.Balance(2, "BTC")
	if !eqAmt(buyerAvail, 11, "BTC") {
		t.Fatalf("buyer btc double-released by dispute: %v", buyerAvail)
	}
	escrow, _, _ := l.Balance(ledger.SysOtc, "BTC")
	if escrow.Sign() != 0 {
		t.Fatalf("escrow not cleared after idempotent resolve: %v", escrow)
	}
}

// TestReconcilePerAsset 验证 F3：对账按订单实际资产分别核对托管余额，多资产不漏检。
func TestReconcilePerAsset(t *testing.T) {
	svc, _ := newTestService()
	// 建一个 USDT 资产的卖方广告并吃单（托管 USDT），与默认 BTC 并存。
	ad := &OtcAdvertisement{
		UserID: 1, Side: SideSell, Asset: "USDT", FiatCurrency: "CNY",
		Price: 7, MinAmount: 10, MaxAmount: 5000, PaymentMethods: "bank",
	}
	if err := svc.CreateAdvertisement(ad); err != nil {
		t.Fatalf("create ad: %v", err)
	}
	if _, err := svc.TakeOrder(ad.ID, 2, 700, "bank"); err != nil { // 100 USDT
		t.Fatalf("take: %v", err)
	}
	escrow, stuck, err := svc.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// 默认 BTC 无订单也应被核对（恒为 0），USDT 托管 100 也应被核对。
	if _, ok := escrow["BTC"]; !ok {
		t.Fatalf("BTC escrow missing from reconcile: %v", escrow)
	}
	usdt, ok := escrow["USDT"]
	if !ok {
		t.Fatalf("USDT escrow missing from reconcile (F3 misbucket): %v", escrow)
	}
	if !eqAmt(usdt, 100, "USDT") {
		t.Fatalf("USDT escrow wrong: %v", usdt)
	}
	if len(stuck) != 1 {
		t.Fatalf("expected 1 stuck order (pending), got %d", len(stuck))
	}
}
