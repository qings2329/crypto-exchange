package lending

import (
	"math/big"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

func newTestLendingService() (*Service, *MemStore) {
	store := NewMemStore()
	l := ledger.New() // in-memory ledger
	cfg := Config{
		BaseInterestRate: 0.05,
		MaxInterestRate:  1.0,
		MinLendAmount:    0,
		MinBorrowAmount:  0,
	}
	svc := NewService(store, l, cfg, nil)
	return svc, store
}

func usdt(human float64) settlement.AssetAmount {
	return settlement.AssetAmountFromFloat(human, 8)
}

func TestCreatePool(t *testing.T) {
	svc, _ := newTestLendingService()
	p, err := svc.CreatePool("USDT", 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if p.Asset != "USDT" || p.CollateralReq != 1.5 {
		t.Fatalf("unexpected pool: %+v", p)
	}
	// Duplicate should fail
	_, err = svc.CreatePool("USDT", 1.5)
	if err == nil {
		t.Fatal("expected error for duplicate pool")
	}
}

func TestLendDeposit(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)

	order, err := svc.Lend(1, p.ID, usdt(1000))
	if err != nil {
		t.Fatal(err)
	}
	if order.Amount.HumanString() != "1000" {
		t.Fatalf("expected 1000, got %s", order.Amount.HumanString())
	}
	if order.UserID != 1 {
		t.Fatalf("expected user 1, got %d", order.UserID)
	}

	// Verify pool updated
	p2, _ := svc.store.GetPool(p.ID)
	if p2.TotalSupply.HumanString() != "1000" {
		t.Fatalf("expected total_supply=1000, got %s", p2.TotalSupply.HumanString())
	}
}

func TestLendPoolNotActive(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	p.Status = PoolClosed
	svc.store.UpdatePool(p)

	_, err := svc.Lend(1, p.ID, usdt(100))
	if err != ErrPoolNotActive {
		t.Fatalf("expected ErrPoolNotActive, got %v", err)
	}
}

func TestBorrowBasic(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)

	// Seed pool with liquidity
	svc.Lend(100, p.ID, usdt(10000))

	// Borrow: need 150% collateral for 1000 USDT
	borrowAmt := usdt(1000)
	collateral := usdt(1500) // 150%
	order, err := svc.Borrow(2, p.ID, borrowAmt, collateral)
	if err != nil {
		t.Fatal(err)
	}
	if order.Amount.HumanString() != "1000" {
		t.Fatalf("expected 1000, got %s", order.Amount.HumanString())
	}

	// Verify pool
	p2, _ := svc.store.GetPool(p.ID)
	if p2.TotalBorrow.HumanString() != "1000" {
		t.Fatalf("expected total_borrow=1000, got %s", p2.TotalBorrow.HumanString())
	}
}

func TestBorrowInsufficientCollateral(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(10000))

	// Try to borrow 1000 with only 1400 collateral (140% < 150%)
	_, err := svc.Borrow(2, p.ID, usdt(1000), usdt(1400))
	if err != ErrInsufficientCollateral {
		t.Fatalf("expected ErrInsufficientCollateral, got %v", err)
	}
}

func TestBorrowInsufficientLiquidity(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(100))

	// Try to borrow 200 when only 100 available
	_, err := svc.Borrow(2, p.ID, usdt(200), usdt(300))
	if err != ErrInsufficientLiquidity {
		t.Fatalf("expected ErrInsufficientLiquidity, got %v", err)
	}
}

func TestRepay(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(10000))

	borrow, _ := svc.Borrow(2, p.ID, usdt(1000), usdt(1500))

	repaid, err := svc.Repay(2, borrow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaid.Status != "repaid" {
		t.Fatalf("expected repaid, got %s", repaid.Status)
	}
}

func TestRepayNotOwner(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(10000))

	borrow, _ := svc.Borrow(2, p.ID, usdt(1000), usdt(1500))

	_, err := svc.Repay(99, borrow.ID)
	if err != ErrNotOwner {
		t.Fatalf("expected ErrNotOwner, got %v", err)
	}
}

func TestRepayAlreadyRepaid(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(10000))

	borrow, _ := svc.Borrow(2, p.ID, usdt(1000), usdt(1500))
	svc.Repay(2, borrow.ID)

	_, err := svc.Repay(2, borrow.ID)
	if err != ErrAlreadyRepaid {
		t.Fatalf("expected ErrAlreadyRepaid, got %v", err)
	}
}

func TestWithdraw(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)

	order, _ := svc.Lend(1, p.ID, usdt(1000))

	withdrawn, err := svc.Withdraw(1, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.Status != "withdrawn" {
		t.Fatalf("expected withdrawn, got %s", withdrawn.Status)
	}
}

func TestWithdrawNotOwner(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	order, _ := svc.Lend(1, p.ID, usdt(1000))

	_, err := svc.Withdraw(99, order.ID)
	if err != ErrNotOwner {
		t.Fatalf("expected ErrNotOwner, got %v", err)
	}
}

func TestAccrueInterest(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(10000))

	// Borrow 1 hour ago
	borrow, _ := svc.Borrow(2, p.ID, usdt(1000), usdt(1500))

	// Simulate 1 hour later
	now := time.Now().Add(1 * time.Hour)
	err := svc.Accrue(now)
	if err != nil {
		t.Fatal(err)
	}

	// Verify interest accrued
	borrow2, _ := svc.store.GetBorrowOrder(borrow.ID)
	if borrow2.InterestAcc.Sign() <= 0 {
		t.Fatal("expected positive interest after 1 hour")
	}
	t.Logf("interest after 1 hour: %s", borrow2.InterestAcc.HumanString())
}

func TestCalcInterestRate(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	p.TotalSupply = usdt(10000)
	p.TotalBorrow = usdt(5000) // 50% utilization

	rate := svc.CalcInterestRate(p)
	// 50% util: 0.05 + 0.5*(1.0-0.05) = 0.05 + 0.475 = 0.525
	if rate < 0.52 || rate > 0.53 {
		t.Fatalf("expected ~0.525, got %f", rate)
	}
}

func TestPoolInfo(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(1, p.ID, usdt(5000))

	info, err := svc.PoolInfo(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info["asset"] != "USDT" {
		t.Fatalf("expected USDT, got %v", info["asset"])
	}
	if info["lenders"] != 1 {
		t.Fatalf("expected 1 lender, got %v", info["lenders"])
	}
}

func TestLendWithdrawRoundtrip(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)

	// Lend then withdraw
	order, _ := svc.Lend(1, p.ID, usdt(2000))
	svc.Withdraw(1, order.ID)

	// Pool should be empty
	p2, _ := svc.store.GetPool(p.ID)
	if p2.TotalSupply.Sign() != 0 {
		t.Fatalf("expected empty pool after withdraw, got %s", p2.TotalSupply.HumanString())
	}
}

func TestBorrowRepayRoundtrip(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(50000))

	borrow, _ := svc.Borrow(2, p.ID, usdt(5000), usdt(7500))
	svc.Repay(2, borrow.ID)

	// Pool should be restored
	p2, _ := svc.store.GetPool(p.ID)
	if p2.TotalBorrow.Sign() != 0 {
		t.Fatalf("expected zero borrow after repay, got %s", p2.TotalBorrow.HumanString())
	}
}

func TestMultipleLenders(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)

	svc.Lend(1, p.ID, usdt(3000))
	svc.Lend(2, p.ID, usdt(7000))

	p2, _ := svc.store.GetPool(p.ID)
	if p2.TotalSupply.HumanString() != "10000" {
		t.Fatalf("expected 10000, got %s", p2.TotalSupply.HumanString())
	}

	orders, _ := svc.store.ListLendOrdersByPool(p.ID)
	if len(orders) != 2 {
		t.Fatalf("expected 2 lend orders, got %d", len(orders))
	}
}

func TestEmptyPoolNoBorrow(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)

	_, err := svc.Borrow(2, p.ID, usdt(100), usdt(150))
	if err != ErrInsufficientLiquidity {
		t.Fatalf("expected ErrInsufficientLiquidity, got %v", err)
	}
}

// TestInterestAccumulationOverTime verifies interest grows with time
func TestInterestAccumulationOverTime(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(100000))

	borrow, _ := svc.Borrow(2, p.ID, usdt(10000), usdt(15000))

	// Accrue at t+1h
	svc.Accrue(time.Now().Add(1 * time.Hour))
	borrow1, _ := svc.store.GetBorrowOrder(borrow.ID)

	// Accrue at t+2h
	svc.Accrue(time.Now().Add(2 * time.Hour))
	borrow2, _ := svc.store.GetBorrowOrder(borrow.ID)

	if borrow2.InterestAcc.Cmp(borrow1.InterestAcc) <= 0 {
		t.Fatal("expected more interest at t+2h than t+1h")
	}
}

// TestZeroAmountBorrow ensures zero-amount borrow is rejected
func TestZeroAmountBorrow(t *testing.T) {
	svc, _ := newTestLendingService()
	p, _ := svc.CreatePool("USDT", 1.5)
	svc.Lend(100, p.ID, usdt(10000))

	_, err := svc.Borrow(2, p.ID, settlement.NewAssetAmount(big.NewInt(0), 8), usdt(150))
	if err != ErrBelowMinAmount {
		t.Fatalf("expected ErrBelowMinAmount, got %v", err)
	}
}
