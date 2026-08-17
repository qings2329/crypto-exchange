package wealth

import (
	"math"
	"testing"
	"time"

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
		_ = l.Deposit(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed")
	}
	return NewService(store, l, Config{}, nil), l
}

func mustProduct(s *Service, typ ProductType, rate float64, duration int, min float64) *WealthProduct {
	p := &WealthProduct{
		Name:         "test", Asset: "USDT", Type: typ,
		AnnualRate: rate, DurationDays: duration, MinAmount: min,
	}
	if err := s.CreateProduct(p); err != nil {
		panic(err)
	}
	return p
}

func TestSubscribeDeductsAndRedeemCurrent(t *testing.T) {
	svc, l := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.0, 0, 100)

	// 申购 1000，可用扣 1000，托管 SysWealth +1000。
	h, err := svc.Subscribe(1, p.ID, 1000)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if avail, _, _ := l.Balance(1, "USDT"); avail.Cmp(settlement.AssetAmountFromFloat(99000, settlement.AssetDecimalsByName("USDT"))) < 0 {
		t.Fatalf("principal not deducted: avail=%v", avail)
	}
	if sys, _, _ := l.Balance(ledger.SysWealth, "USDT"); sys.Cmp(settlement.AssetAmountFromFloat(999, settlement.AssetDecimalsByName("USDT"))) < 0 {
		t.Fatalf("custody not credited: sys=%v", sys)
	}

	// 活期立即赎回：收益≈0，仅回本金。
	if _, err := svc.Redeem(1, h.ID); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if avail, _, _ := l.Balance(1, "USDT"); avail.Cmp(settlement.AssetAmountFromFloat(99999, settlement.AssetDecimalsByName("USDT"))) < 0 {
		t.Fatalf("redeem did not return principal: avail=%v", avail)
	}
	if sys, _, _ := l.Balance(ledger.SysWealth, "USDT"); sys.Sign() > 0 {
		t.Fatalf("custody not drained: sys=%v", sys)
	}
}

func TestRedeemPaysYield(t *testing.T) {
	svc, l := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.365, 0, 100) // 1% 每天（8760h 对应约 0.1?）简化：365% 年化

	// 直接构造一笔"已持有 1 年"的持仓以稳定测应计收益。
	store := svc.store
	h := &WealthHolding{UserID: 1, ProductID: p.ID, Asset: "USDT",
		Principal: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")),
		Status:    HoldingActive,
		CreatedAt: time.Now().Add(-365 * 24 * time.Hour), LastAccrualAt: time.Now().Add(-365 * 24 * time.Hour)}
	if err := store.CreateHolding(h); err != nil {
		t.Fatalf("seed holding: %v", err)
	}
	before, _, _ := l.Balance(1, "USDT")
	if _, err := svc.Redeem(1, h.ID); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	after, _, _ := l.Balance(1, "USDT")
	// 年化 0.365，持有 1 年，收益≈365；本金 1000 + 收益 ≈1365。
	diff := after.HumanFloat() - before.HumanFloat()
	if diff < 1364 || diff > 1366 {
		t.Fatalf("yield payout wrong: got %.4f want ~1365 (before=%.2f after=%.2f)", diff, before.HumanFloat(), after.HumanFloat())
	}
}

func TestFixedLockedBeforeMaturity(t *testing.T) {
	svc, _ := newTestService()
	p := mustProduct(svc, TypeFixed, 0.06, 30, 100)

	h, err := svc.Subscribe(1, p.ID, 1000)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// 刚申购，未到期，应拒绝赎回。
	if _, err := svc.Redeem(1, h.ID); err != ErrLocked {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestBelowMinAndInsufficient(t *testing.T) {
	svc, _ := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.03, 0, 1000)

	if _, err := svc.Subscribe(1, p.ID, 100); err != ErrBelowMinAmount {
		t.Fatalf("expected ErrBelowMinAmount, got %v", err)
	}

	// 用户可用 100000 USDT，申购超大额应余额不足。
	if _, err := svc.Subscribe(1, p.ID, 1e9); err != ErrInsufficientBal {
		t.Fatalf("expected ErrInsufficientBal, got %v", err)
	}
}

func TestAccrueUpdatesYield(t *testing.T) {
	svc, l := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.876, 0, 100) // 876% 年化 => 1% 每小时

	store := svc.store
	h := &WealthHolding{UserID: 2, ProductID: p.ID, Asset: "USDT",
		Principal: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")),
		Status:    HoldingActive,
		CreatedAt: time.Now().Add(-100 * time.Hour), LastAccrualAt: time.Now().Add(-100 * time.Hour)}
	if err := store.CreateHolding(h); err != nil {
		t.Fatalf("seed holding: %v", err)
	}
	now := time.Now()
	total, err := svc.Accrue(now)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	// 100 小时 × 1%/h = 10 收益，加上之前 0。
	if total < 9.9 || total > 10.1 {
		t.Fatalf("accrued wrong: %.4f want ~10", total)
	}
	got, _ := svc.store.GetHolding(h.ID)
	if got.AccruedYield.HumanFloat() < 9.9 || got.AccruedYield.HumanFloat() > 10.1 {
		t.Fatalf("holding accrued_yield wrong: %.4f", got.AccruedYield.HumanFloat())
	}
	_ = l
}

func TestYieldToPure(t *testing.T) {
	h := &WealthHolding{
		Principal:      settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")),
		LastAccrualAt: time.Now().Add(-100 * time.Hour)}
	got := h.YieldTo(time.Now(), 0.876)
	// 年化 0.876 => 0.01% 每小时 => 0.1/小时；100 小时收益 ≈10。
	if got < 9.9 || got > 10.1 {
		t.Fatalf("YieldTo wrong: %.4f want ~10", got)
	}
}

// TestRedeemIdempotent F1：已赎回的持仓再次赎回应被终态短路，本金+收益只兑付一次。
func TestRedeemIdempotent(t *testing.T) {
	svc, l := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.365, 0, 100) // 365% 年化
	store := svc.store
	h := &WealthHolding{UserID: 1, ProductID: p.ID, Asset: "USDT",
		Principal: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")),
		Status:    HoldingActive,
		CreatedAt: time.Now().Add(-365 * 24 * time.Hour), LastAccrualAt: time.Now().Add(-365 * 24 * time.Hour)}
	if err := store.CreateHolding(h); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, _, _ := l.Balance(1, "USDT")
	if _, err := svc.Redeem(1, h.ID); err != nil {
		t.Fatalf("redeem#1: %v", err)
	}
	if _, err := svc.Redeem(1, h.ID); err != ErrAlreadyRedeemed {
		t.Fatalf("redeem#2: expected ErrAlreadyRedeemed, got %v", err)
	}
	after, _, _ := l.Balance(1, "USDT")
	diff := after.HumanFloat() - before.HumanFloat()
	// 仅兑付一次：本金 1000 + 收益≈365 ≈ 1365，不得翻倍。
	if diff < 1364 || diff > 1366 {
		t.Fatalf("idempotent redeem paid wrong amount: diff=%.4f want ~1365", diff)
	}
}

// TestSubscribeInsufficientNoHolding F1：余额不足时不得残留（Funding）持仓。
func TestSubscribeInsufficientNoHolding(t *testing.T) {
	svc, _ := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.03, 0, 1000)
	if _, err := svc.Subscribe(1, p.ID, 1e9); err != ErrInsufficientBal {
		t.Fatalf("expected ErrInsufficientBal, got %v", err)
	}
	list, _ := svc.MyHoldings(1)
	if len(list) != 0 {
		t.Fatalf("insufficient subscribe must not leave holding: %d", len(list))
	}
}

// TestCreateProductRejectsUnsupportedAsset F5-1：未知资产不再静默兜底为 USDT，必须显式拒绝。
func TestCreateProductRejectsUnsupportedAsset(t *testing.T) {
	svc, _ := newTestService()
	p := &WealthProduct{Name: "x", Asset: "FOOCOIN", Type: TypeCurrent, AnnualRate: 0.03, MinAmount: 100}
	if err := svc.CreateProduct(p); err != ErrUnsupportedAsset {
		t.Fatalf("expected ErrUnsupportedAsset, got %v", err)
	}
}

// TestCreateProductRejectsRateOverMax F5-3：年化费率超过安全上界必须拒绝，避免 SysWealth 透支。
func TestCreateProductRejectsRateOverMax(t *testing.T) {
	svc, _ := newTestService()
	p := &WealthProduct{Name: "x", Asset: "USDT", Type: TypeCurrent, AnnualRate: MaxAnnualRate + 1, MinAmount: 100}
	if err := svc.CreateProduct(p); err != ErrInvalidRate {
		t.Fatalf("expected ErrInvalidRate, got %v", err)
	}
}

// TestSubscribeRejectsNaN F5-2：NaN/Inf/<=0 申购额必须被 finitePositive 拒绝（原 <=0 守卫对 NaN/Inf 失效）。
func TestSubscribeRejectsNaN(t *testing.T) {
	svc, _ := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.03, 0, 100)
	for _, amt := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -100} {
		if _, err := svc.Subscribe(1, p.ID, amt); err != ErrInvalidAmount {
			t.Fatalf("amount=%v: expected ErrInvalidAmount, got %v", amt, err)
		}
	}
}

// TestWealthAccrueMovesYieldToPayable F3-1：计息时收益须从 SysWealth 划转到
// SysWealthYieldPayable，使 SysWealth 余额恒等于「本金 - 已计收益」，赎回不再凭空兑付收益。
func TestWealthAccrueMovesYieldToPayable(t *testing.T) {
	svc, l := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.876, 0, 100) // 876% 年化 => 1%/h
	h, err := svc.Subscribe(1, p.ID, 1000)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// 申购后本金 1000 入 SysWealth，应计收益负债账户为空。
	if sys, _, _ := l.Balance(ledger.SysWealth, "USDT"); !eqAmt(sys, 1000, "USDT") {
		t.Fatalf("custody wrong: %v want 1000", sys)
	}
	if pay, _, _ := l.Balance(ledger.SysWealthYieldPayable, "USDT"); pay.Sign() != 0 {
		t.Fatalf("payable should be 0 before accrue: %v", pay)
	}
	// 让持仓从 100 小时前起算，计息 100h*1%/h ≈ 10 收益。
	h.CreatedAt = time.Now().Add(-100 * time.Hour)
	h.LastAccrualAt = h.CreatedAt
	_ = svc.store.UpdateHolding(h)
	if _, err := svc.Accrue(time.Now()); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	got, _ := svc.store.GetHolding(h.ID)
	// 应计收益负债账户余额 == 持仓已计收益（精确定点对账）。
	if pay, _, _ := l.Balance(ledger.SysWealthYieldPayable, "USDT"); pay.Cmp(got.AccruedYield) != 0 {
		t.Fatalf("SysWealthYieldPayable %v != accrued yield %v", pay, got.AccruedYield)
	}
	// SysWealth 余额 == 本金 - 已计收益（invariant）。
	wantSys := h.Principal.Sub(got.AccruedYield)
	if sys, _, _ := l.Balance(ledger.SysWealth, "USDT"); sys.Cmp(wantSys) != 0 {
		t.Fatalf("SysWealth %v != principal-yield %v", sys, wantSys)
	}
}

// TestWealthAccrueIntegerExact #47：利息整数化——按定点整数运算，增量计息精确累加、无浮点尾差。
func TestWealthAccrueIntegerExact(t *testing.T) {
	svc, _ := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.876, 0, 100) // 876% 年化 => 1% 每小时
	store := svc.store
	base := time.Now()
	h := &WealthHolding{UserID: 2, ProductID: p.ID, Asset: "USDT",
		Principal: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")),
		Status:    HoldingActive, CreatedAt: base, LastAccrualAt: base}
	if err := store.CreateHolding(h); err != nil {
		t.Fatalf("seed holding: %v", err)
	}
	// 第一次计息：100 小时 => 恰好 10.000000 USDT（定点整数，无尾差）。
	if _, err := svc.Accrue(base.Add(100 * time.Hour)); err != nil {
		t.Fatalf("accrue#1: %v", err)
	}
	got, _ := store.GetHolding(h.ID)
	if !eqAmt(got.AccruedYield, 10, "USDT") {
		t.Fatalf("first accrual %v want exactly 10 (integer, no float drift)", got.AccruedYield)
	}
	// 第二次计息：再 100 小时 => 再恰好 10，累计 20（增量精确累加）。
	if _, err := svc.Accrue(base.Add(200 * time.Hour)); err != nil {
		t.Fatalf("accrue#2: %v", err)
	}
	got, _ = store.GetHolding(h.ID)
	if !eqAmt(got.AccruedYield, 20, "USDT") {
		t.Fatalf("second accrual total %v want exactly 20", got.AccruedYield)
	}
}
