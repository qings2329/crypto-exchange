package margin

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
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = l.Deposit(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed")
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
	if a.Debt.HumanFloat() != 1.0 || a.Leverage != 5 {
		t.Fatalf("unexpected account: %+v", a)
	}
	// 抵押 = 1/5 = 0.2 USDT 应被冻结，BTC 1.0 应进可用。
	if _, frozen, _ := l.Balance(uid, "USDT"); frozen.Cmp(settlement.AssetAmountFromFloat(0.199999, settlement.AssetDecimalsByName("USDT"))) < 0 {
		t.Fatalf("collateral not frozen: frozen=%v", frozen)
	}
	if avail, _, _ := l.Balance(uid, "BTC"); avail.Cmp(settlement.AssetAmountFromFloat(0.999999, settlement.AssetDecimalsByName("BTC"))) < 0 {
		t.Fatalf("borrowed BTC not credited: avail=%v", avail)
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
	if _, frozen, _ := l.Balance(uid, "USDT"); frozen.Sign() > 0 {
		t.Fatalf("collateral not unfrozen: frozen=%v", frozen)
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
	if a.InterestAccrued.HumanFloat() < want-1e-9 || a.InterestAccrued.HumanFloat() > want+1e-9 {
		t.Fatalf("interest accrued = %.8f, want %.8f", a.InterestAccrued.HumanFloat(), want)
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
	if avail, _, _ := l.Balance(uid, "BTC"); avail.Sign() > 0 {
		t.Fatalf("borrowed BTC not seized: avail=%v", avail)
	}
	// 抵押部分罚没入保险基金，剩余应解冻。
	if _, frozen, _ := l.Balance(uid, "USDT"); frozen.Sign() > 0 {
		t.Fatalf("collateral still frozen after liquidation: frozen=%v", frozen)
	}
	ins, _, _ := l.Balance(ledger.SysInsurance, "USDT")
	if ins.Sign() <= 0 {
		t.Fatalf("liquidation penalty not transferred to insurance: ins=%v", ins)
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

// TestBorrowRejectDuplicateActive F1：同一 (user, asset) 已存在活跃账户时拒绝重复开仓，避免覆盖式双借双冻。
func TestBorrowRejectDuplicateActive(t *testing.T) {
	svc, _ := newTestService()
	if _, err := svc.Borrow(1, "BTC", 1.0, 5); err != nil {
		t.Fatalf("borrow#1: %v", err)
	}
	if _, err := svc.Borrow(1, "BTC", 1.0, 5); err != ErrAlreadyBorrowed {
		t.Fatalf("borrow#2: expected ErrAlreadyBorrowed, got %v", err)
	}
	list, _ := svc.Accounts(1)
	n := 0
	for _, a := range list {
		if a.Asset == "BTC" {
			n++
			if a.Debt.HumanFloat() != 1.0 {
				t.Fatalf("unexpected debt after duplicate borrow: %v", a.Debt.HumanFloat())
			}
		}
	}
	if n != 1 {
		t.Fatalf("expected 1 BTC account (no overwrite double-borrow), got %d", n)
	}
}

// TestRepayIdempotent F1：已还清关闭的账户再次还款应被终态短路，不双扣。
func TestRepayIdempotent(t *testing.T) {
	svc, l := newTestService()
	const uid = int64(1)
	if _, err := svc.Borrow(uid, "BTC", 1.0, 5); err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if err := svc.Repay(uid, "BTC", 1.0); err != nil {
		t.Fatalf("repay#1: %v", err)
	}
	if err := svc.Repay(uid, "BTC", 1.0); err != ErrAccountLiquidated {
		t.Fatalf("repay#2: expected ErrAccountLiquidated, got %v", err)
	}
	// BTC 可用回到 0（借入 1.0 后全额还回，无双扣）。
	if avail, _, _ := l.Balance(uid, "BTC"); avail.Sign() != 0 {
		t.Fatalf("repay not idempotent: BTC avail=%v", avail)
	}
}

// TestLiquidateIdempotent F1：已强平的账户再次强平应被终态短路，不双占双罚。
func TestLiquidateIdempotent(t *testing.T) {
	svc, l := newTestService()
	const uid = int64(3)
	if _, err := svc.Borrow(uid, "BTC", 1.0, 2); err != nil {
		t.Fatalf("borrow: %v", err)
	}
	svc.priceFn = func(asset string) (float64, bool) { return 100.0, true }
	done, err := svc.Liquidate(uid, "BTC")
	if err != nil || !done {
		t.Fatalf("liquidate#1: done=%v err=%v", done, err)
	}
	done, err = svc.Liquidate(uid, "BTC")
	if err != nil || done {
		t.Fatalf("liquidate#2: expected (false,nil), got done=%v err=%v", done, err)
	}
	// 保险基金罚没仅一次：抵押 0.5 * 5% = 0.025 USDT。
	ins, _, _ := l.Balance(ledger.SysInsurance, "USDT")
	if !eqAmt(ins, 0.025, "USDT") {
		t.Fatalf("liquidate not idempotent: insurance=%v want 0.025", ins)
	}
}

// TestBorrowRejectsUnsupportedAsset F5-1：未知资产不得借入（防凭空铸造）。
func TestBorrowRejectsUnsupportedAsset(t *testing.T) {
	svc, _ := newTestService()
	if _, err := svc.Borrow(1, "XYZ", 1.0, 2); err != ErrUnsupportedAsset {
		t.Fatalf("expected ErrUnsupportedAsset, got %v", err)
	}
}

// TestRepayRejectsNaN F5-2：NaN/Inf 金额不得作为还款（AssetAmountFromFloat 会静默归零）。
func TestRepayRejectsNaN(t *testing.T) {
	svc, _ := newTestService()
	if err := svc.Repay(1, "BTC", math.NaN()); err != ErrAmountMustBePositive {
		t.Fatalf("expected ErrAmountMustBePositive for NaN, got %v", err)
	}
	if err := svc.Repay(1, "BTC", math.Inf(1)); err != ErrAmountMustBePositive {
		t.Fatalf("expected ErrAmountMustBePositive for +Inf, got %v", err)
	}
}
