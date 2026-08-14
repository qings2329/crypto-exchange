package margin

import (
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
)

func newTestService() (*Service, *ledger.Ledger) {
	store := NewMemStore()
	l := ledger.New()
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = l.Deposit(uid, "USDT", 100000, "seed")
	}
	cfg := Config{
		MaxLeverage:      5,
		HourlyRate:       0.0001, // 0.01%/h
		MaintenanceRatio: 1.05,
		CollateralAsset:  "USDT",
		AccrueInterval:   time.Second,
	}
	return NewService(store, l, cfg, nil, nil), l
}

func TestBorrowAndRepay(t *testing.T) {
	svc, l := newTestService()
	const uid = int64(1)

	a, err := svc.Borrow(uid, "BTC", 1.0, 5)
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if a.Debt != 1.0 || a.Leverage != 5 {
		t.Fatalf("unexpected account: %+v", a)
	}
	// 抵押 = 1/5 = 0.2 USDT 应被冻结，BTC 1.0 应进可用。
	if _, frozen, _ := l.Balance(uid, "USDT"); frozen < 0.199999 {
		t.Fatalf("collateral not frozen: frozen=%.6f", frozen)
	}
	if avail, _, _ := l.Balance(uid, "BTC"); avail < 0.999999 {
		t.Fatalf("borrowed BTC not credited: avail=%.6f", avail)
	}

	// 全额还币：债务 + 0 利息（刚借未计息）。
	if err := svc.Repay(uid, "BTC", 1.0); err != nil {
		t.Fatalf("repay: %v", err)
	}
	a, err = svc.Account(uid, "BTC")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if a.Status != StatusClosed {
		t.Fatalf("expected closed, got %s", a.Status)
	}
	if _, frozen, _ := l.Balance(uid, "USDT"); frozen > 1e-9 {
		t.Fatalf("collateral not unfrozen: frozen=%.6f", frozen)
	}
}

func TestBorrowInsufficientCollateral(t *testing.T) {
	svc, _ := newTestService()
	// 借 100 万 BTC 需 20 万 USDT 抵押，demo 仅 10 万，应失败。
	if _, err := svc.Borrow(1, "BTC", 1_000_000, 5); err != ErrInsufficientCollateral {
		t.Fatalf("expected ErrInsufficientCollateral, got %v", err)
	}
}

func TestBorrowOverMaxLeverage(t *testing.T) {
	svc, _ := newTestService()
	if _, err := svc.Borrow(1, "BTC", 1.0, 10); err != ErrOverMaxLeverage {
		t.Fatalf("expected ErrOverMaxLeverage, got %v", err)
	}
}

func TestAccrueInterest(t *testing.T) {
	svc, _ := newTestService()
	a, err := svc.Borrow(2, "ETH", 10.0, 2)
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	// 模拟已过去 10 小时。
	a.LastAccrual = time.Now().Add(-10 * time.Hour)
	svc.accrue(a)
	// 利息 = 10 * 0.0001 * 10 = 0.01
	want := 10.0 * 0.0001 * 10
	if a.InterestAccrued < want-1e-9 || a.InterestAccrued > want+1e-9 {
		t.Fatalf("interest accrued = %.8f, want %.8f", a.InterestAccrued, want)
	}
}

func TestLiquidationPriceAndLiquidate(t *testing.T) {
	svc, l := newTestService()
	const uid = int64(3)
	// 借 1 BTC，杠杆 2 -> 抵押 0.5 USDT。
	a, err := svc.Borrow(uid, "BTC", 1.0, 2)
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	// 强平价 = 0.5 / (1 * 1.05) ≈ 0.476
	lp, err := svc.LiquidationPrice(uid, "BTC")
	if err != nil {
		t.Fatalf("liq price: %v", err)
	}
	if lp < 0.47 || lp > 0.49 {
		t.Fatalf("liq price = %.4f, want ~0.476", lp)
	}

	// 注入价格函数：标记价远高于强平价 -> 应强平。
	svc.priceFn = func(asset string) (float64, bool) { return 100.0, true }
	done, err := svc.Liquidate(uid, "BTC")
	if err != nil {
		t.Fatalf("liquidate: %v", err)
	}
	if !done {
		t.Fatal("expected liquidation to trigger")
	}
	// 借出 BTC 应被收回（可用归零）。
	if avail, _, _ := l.Balance(uid, "BTC"); avail > 1e-9 {
		t.Fatalf("borrowed BTC not seized: avail=%.6f", avail)
	}
	// 抵押部分罚没入保险基金，剩余应解冻。
	if _, frozen, _ := l.Balance(uid, "USDT"); frozen > 1e-9 {
		t.Fatalf("collateral still frozen after liquidation: frozen=%.6f", frozen)
	}
	ins, _, _ := l.Balance(ledger.SysInsurance, "USDT")
	if ins <= 0 {
		t.Fatalf("liquidation penalty not transferred to insurance: ins=%.6f", ins)
	}
	a, _ = svc.Account(uid, "BTC")
	if a.Status != StatusLiquidated {
		t.Fatalf("expected liquidated, got %s", a.Status)
	}
}

func TestLiquidateNoPriceSkips(t *testing.T) {
	svc, _ := newTestService()
	if _, err := svc.Borrow(4, "BTC", 1.0, 2); err != nil {
		t.Fatalf("borrow: %v", err)
	}
	// priceFn 为 nil -> 不应强平，也不报错。
	done, err := svc.Liquidate(4, "BTC")
	if err != nil {
		t.Fatalf("liquidate nil price: %v", err)
	}
	if done {
		t.Fatal("expected no liquidation without price feed")
	}
}
