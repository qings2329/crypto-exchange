package wealth

import (
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
)

func newTestService() (*Service, *ledger.Ledger) {
	store := NewMemStore()
	l := ledger.New()
	for _, uid := range []int64{1, 2} {
		_ = l.Deposit(uid, "USDT", 100000, "seed")
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
	if avail, _, _ := l.Balance(1, "USDT"); avail < 99000-1e-6 {
		t.Fatalf("principal not deducted: avail=%.4f", avail)
	}
	if sys, _, _ := l.Balance(ledger.SysWealth, "USDT"); sys < 999-1e-6 {
		t.Fatalf("custody not credited: sys=%.4f", sys)
	}

	// 活期立即赎回：收益≈0，仅回本金。
	if _, err := svc.Redeem(1, h.ID); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if avail, _, _ := l.Balance(1, "USDT"); avail < 99999-1e-6 {
		t.Fatalf("redeem did not return principal: avail=%.4f", avail)
	}
	if sys, _, _ := l.Balance(ledger.SysWealth, "USDT"); sys > 1e-6 {
		t.Fatalf("custody not drained: sys=%.4f", sys)
	}
}

func TestRedeemPaysYield(t *testing.T) {
	svc, l := newTestService()
	p := mustProduct(svc, TypeCurrent, 0.365, 0, 100) // 1% 每天（8760h 对应约 0.1?）简化：365% 年化

	// 直接构造一笔"已持有 1 年"的持仓以稳定测应计收益。
	store := svc.store
	h := &WealthHolding{UserID: 1, ProductID: p.ID, Principal: 1000, Status: HoldingActive,
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
	if after-before < 1364 || after-before > 1366 {
		t.Fatalf("yield payout wrong: got %.4f want ~1365 (before=%.2f after=%.2f)", after-before, before, after)
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
	h := &WealthHolding{UserID: 2, ProductID: p.ID, Principal: 1000, Status: HoldingActive,
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
	if got.AccruedYield < 9.9 || got.AccruedYield > 10.1 {
		t.Fatalf("holding accrued_yield wrong: %.4f", got.AccruedYield)
	}
	_ = l
}

func TestYieldToPure(t *testing.T) {
	h := &WealthHolding{Principal: 1000, LastAccrualAt: time.Now().Add(-100 * time.Hour)}
	// 年化 0.876 => 0.01% 每小时 => 0.1/小时；100 小时收益 ≈10。
	got := h.YieldTo(time.Now(), 0.876)
	if got < 9.9 || got > 10.1 {
		t.Fatalf("YieldTo wrong: %.4f want ~10", got)
	}
}
