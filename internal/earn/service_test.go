package earn

import (
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

func amt(human float64, dec int) settlement.AssetAmount {
	a, _ := settlement.AssetAmountFromFloatSafe(human, dec)
	return a
}

// newTestService 构造内存 store + 真实账本的测试服务；uid 预充值 USDT 100000 / BTC 10。
func newTestService(t *testing.T) (*Service, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	for _, uid := range []int64{1, 2} {
		if err := l.ReceiveOnChain(uid, "USDT", amt(100000, 6), "seed"); err != nil {
			t.Fatalf("seed usdt: %v", err)
		}
		if err := l.ReceiveOnChain(uid, "BTC", amt(10, 8), "seed"); err != nil {
			t.Fatalf("seed btc: %v", err)
		}
	}
	return NewService(NewMemStore(), l, Config{}, nil), l
}

func seedProduct(t *testing.T, s *Service, termDays int) *EarnProduct {
	t.Helper()
	p := &EarnProduct{Name: "test", Asset: "USDT", TermDays: termDays, APY: 0.08, MinAmount: 10}
	if err := s.CreateProduct(p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	return p
}

// eqAmt 定点相等断言（按资产小数位）。
func eqAmt(a settlement.AssetAmount, human float64, asset string) bool {
	want := amt(human, settlement.AssetDecimalsByName(asset))
	return a.Cmp(want) == 0
}

func TestEarnSubscribeAndRedeemFlexible(t *testing.T) {
	s, l := newTestService(t)
	p := seedProduct(t, s, 0)

	sub, err := s.Subscribe(1, p.ID, 5000, true)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	avail, _, _ := l.Balance(1, "USDT")
	if !eqAmt(avail, 95000, "USDT") {
		t.Fatalf("principal not locked: avail=%v", avail.HumanFloat())
	}

	// 活期随时可赎回；计息窗口≈0，收益截断为 0
	if _, err := s.Redeem(1, sub.ID); err != nil {
		t.Fatalf("redeem flexible: %v", err)
	}
	avail, _, _ = l.Balance(1, "USDT")
	if !eqAmt(avail, 100000, "USDT") {
		t.Fatalf("redeem payout wrong: avail=%v", avail.HumanFloat())
	}
	// 终态幂等：重复赎回拒绝
	if _, err := s.Redeem(1, sub.ID); err != ErrAlreadyRedeemed {
		t.Fatalf("expect ErrAlreadyRedeemed, got %v", err)
	}
}

func TestEarnSubscribeGuards(t *testing.T) {
	s, _ := newTestService(t)
	p := seedProduct(t, s, 30)

	if _, err := s.Subscribe(1, p.ID, 5, true); err != ErrBelowMinAmount {
		t.Fatalf("expect below min, got %v", err)
	}
	if _, err := s.Subscribe(1, p.ID, 100, false); err != ErrAgreementRequired {
		t.Fatalf("expect agreement required, got %v", err)
	}
	if _, err := s.Subscribe(1, p.ID, -3, true); err != ErrInvalidAmount {
		t.Fatalf("expect invalid amount, got %v", err)
	}
	if _, err := s.Subscribe(1, 999, 100, true); err != ErrProductNotFound {
		t.Fatalf("expect product not found, got %v", err)
	}
	pBig := &EarnProduct{Name: "big", Asset: "USDT", APY: 0.01, MinAmount: 1, MaxAmount: 1000}
	if err := s.CreateProduct(pBig); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Subscribe(1, pBig.ID, 2000, true); err != ErrAboveMaxAmount {
		t.Fatalf("expect above max, got %v", err)
	}
	if _, err := s.Subscribe(2, p.ID, 1e12, true); err != ErrInsufficientBal {
		t.Fatalf("expect insufficient, got %v", err)
	}
	// 非白名单资产拒绝（F5）
	pBad := &EarnProduct{Name: "bad", Asset: "FAKECOIN", APY: 0.1, MinAmount: 1}
	if err := s.CreateProduct(pBad); err != ErrUnsupportedAsset {
		t.Fatalf("expect unsupported asset, got %v", err)
	}
}

func TestEarnFixedLockedBeforeMaturity(t *testing.T) {
	s, _ := newTestService(t)
	base := time.Now()
	now := base
	s.SetNowFunc(func() time.Time { return now })

	p := seedProduct(t, s, 30)
	sub, err := s.Subscribe(1, p.ID, 1000, true)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := s.Redeem(1, sub.ID); err != ErrLocked {
		t.Fatalf("expect locked, got %v", err)
	}
	now = base.AddDate(0, 0, 31) // 越过锁定期（统一时钟）
	if _, err := s.Redeem(1, sub.ID); err != nil {
		t.Fatalf("redeem after maturity: %v", err)
	}
}

func TestEarnAccrualBooksYieldPayable(t *testing.T) {
	s, l := newTestService(t)
	base := time.Now()
	now := base
	s.SetNowFunc(func() time.Time { return now })

	p := seedProduct(t, s, 0)
	sub, err := s.Subscribe(1, p.ID, 10000, true)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// 推进 365 天 → 收益 = 10000 * 8% = 800 USDT
	now = base.AddDate(1, 0, 0)
	total, err := s.AccrueAll(now)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if !eqAmt(total, 800, "USDT") {
		t.Fatalf("accrued total=%v want 800", total.HumanFloat())
	}
	// 复式校验：SysWealthYieldPayable 应有 ≈800 负债（F3-1 拆分记账）
	payable, _, _ := l.Balance(ledger.SysWealthYieldPayable, "USDT")
	if !eqAmt(payable, 800, "USDT") {
		t.Fatalf("yield payable=%v want 800", payable.HumanFloat())
	}
	// 读路径投影不改账本
	views, _ := s.MySubscriptions(1)
	if len(views) != 1 || views[0].Accrued < 799.99 || views[0].Accrued > 800.01 {
		t.Fatalf("view accrued=%v want ~800", views[0].Accrued)
	}
	// 赎回兑付本金+收益
	if _, err := s.Redeem(1, sub.ID); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	avail, _, _ := l.Balance(1, "USDT")
	if !eqAmt(avail, 100800, "USDT") {
		t.Fatalf("final avail=%v want 100800", avail.HumanFloat())
	}
	payable, _, _ = l.Balance(ledger.SysWealthYieldPayable, "USDT")
	if !payable.IsZero() {
		t.Fatalf("payable should be zero after redeem, got %v", payable.HumanFloat())
	}
	if !l.IsBalanced() {
		t.Fatal("ledger unbalanced")
	}
}

// ongoingProject 创建一个进行中的示例项目（usdt 池 15% / btc 池 5%）。
func ongoingProject(t *testing.T, s *Service) *LaunchProject {
	t.Helper()
	now := func() time.Time { return s.now() }
	p := &LaunchProject{
		Name: "NEW 挖矿", Token: "NEW",
		StartsAt: now().Add(-time.Hour), EndsAt: now().AddDate(1, 0, 0),
		Pools: []LaunchPool{
			{ID: "usdt", Asset: "USDT", APY: 0.15},
			{ID: "btc", Asset: "BTC", APY: 0.05},
		},
	}
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func TestLaunchpadStakeHarvestFlow(t *testing.T) {
	s, l := newTestService(t)
	base := time.Now()
	now := base
	s.SetNowFunc(func() time.Time { return now })

	p := ongoingProject(t, s)
	if _, err := s.FundProject(1, p.ID, 100000); err != nil {
		t.Fatalf("fund: %v", err)
	}

	pos, err := s.Stake(2, p.ID, "usdt", 20000)
	if err != nil {
		t.Fatalf("stake: %v", err)
	}
	stakedAvail, _, _ := l.Balance(2, "USDT")
	if !eqAmt(stakedAvail, 80000, "USDT") {
		t.Fatalf("staked principal not locked: %v", stakedAvail.HumanFloat())
	}
	// 无奖励时领取被拒
	if _, err := s.Harvest(2, pos.ID); err != ErrNothingToHarvest {
		t.Fatalf("expect nothing to harvest, got %v", err)
	}

	// 推进半年：奖励 = 20000 * 15% / 2 = 1500 NEW
	now = base.AddDate(0, 6, 0)
	claimed, err := s.Harvest(2, pos.ID)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	// AddDate 半年 ≈ 184 天：20000 × 15% × 184/365 ≈ 1512（区间断言容忍日历天数差）
	if claimed.HumanFloat() < 1490 || claimed.HumanFloat() > 1520 {
		t.Fatalf("claimed=%v want ~1500 NEW", claimed.HumanFloat())
	}
	newBal, _, _ := l.Balance(2, "NEW")
	if newBal.HumanFloat() < 1490 || newBal.HumanFloat() > 1520 {
		t.Fatalf("user NEW balance=%v", newBal.HumanFloat())
	}
	pool, _, _ := l.Balance(ledger.SysStakingReward, "NEW")
	if pool.HumanFloat() < 98480 || pool.HumanFloat() > 98510 {
		t.Fatalf("reward pool=%v want ~98500", pool.HumanFloat())
	}

	// 全额解押
	if _, err := s.Unstake(2, pos.ID, 0); err != nil {
		t.Fatalf("unstake: %v", err)
	}
	stakedAvail, _, _ = l.Balance(2, "USDT")
	if !eqAmt(stakedAvail, 100000, "USDT") {
		t.Fatalf("after unstake avail=%v", stakedAvail.HumanFloat())
	}
	if dev := s.Reconcile(); len(dev) != 0 {
		t.Fatalf("reconcile deviations: %v", dev)
	}
	if !l.IsBalanced() {
		t.Fatal("ledger unbalanced")
	}
}

func TestLaunchpadStatusGatesAndBudgetFailsafe(t *testing.T) {
	s, _ := newTestService(t)
	base := time.Now()
	now := base
	s.SetNowFunc(func() time.Time { return now })

	p := ongoingProject(t, s)

	// upcoming 项目不可质押
	future := &LaunchProject{
		Name: "future", Token: "NEW",
		StartsAt: base.Add(time.Hour), EndsAt: base.Add(2 * time.Hour),
		Pools: []LaunchPool{{ID: "usdt", Asset: "USDT", APY: 0.1}},
	}
	if err := s.CreateProject(future); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stake(1, future.ID, "usdt", 100); err != ErrProjectNotOngoing {
		t.Fatalf("expect not ongoing, got %v", err)
	}

	// 预算不足 fail-safe：只充 100 NEW，两年利息远超预算 → 领取失败且 Pending 不丢
	if _, err := s.FundProject(1, p.ID, 100); err != nil {
		t.Fatal(err)
	}
	pos, err := s.Stake(1, p.ID, "usdt", 20000) // USDT 池 apy=15%
	if err != nil {
		t.Fatalf("stake: %v", err)
	}
	now = base.AddDate(2, 0, 0) // 两年待领 6000 NEW >> 预算 100
	if _, herr := s.Harvest(1, pos.ID); herr != ErrPoolExhausted {
		t.Fatalf("expect ErrPoolExhausted, got %v", herr)
	}
	pos2, gerr := s.store.GetPosition(pos.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if pos2.RewardsPending.Sign() <= 0 {
		t.Fatal("pending rewards lost on failed harvest")
	}
}

func TestLaunchpadRepeatedStakeNotDedupedByIdempotency(t *testing.T) {
	s, l := newTestService(t)
	p := ongoingProject(t, s)
	if _, err := s.FundProject(1, p.ID, 10_000); err != nil {
		t.Fatal(err)
	}

	// 同仓位连续两笔等额质押：ref 含 seq，不得被账本指纹去重误吞第二笔（F1 回归）
	if _, err := s.Stake(1, p.ID, "usdt", 100); err != nil {
		t.Fatalf("stake#1: %v", err)
	}
	if _, err := s.Stake(1, p.ID, "usdt", 100); err != nil {
		t.Fatalf("stake#2 (equal amount): %v", err)
	}
	custody, _, _ := l.Balance(ledger.SysStaking, "USDT")
	if !eqAmt(custody, 200, "USDT") {
		t.Fatalf("custody=%v want 200 (second stake was deduped!)", custody.HumanFloat())
	}
}

func TestLaunchpadUnstakePartialAndGuards(t *testing.T) {
	s, l := newTestService(t)
	base := time.Now()
	now := base
	s.SetNowFunc(func() time.Time { return now })
	p := ongoingProject(t, s)

	pos, err := s.Stake(2, p.ID, "usdt", 1000)
	if err != nil {
		t.Fatal(err)
	}
	// 超额解押拒绝
	if _, err := s.Unstake(2, pos.ID, 2000); err != ErrInvalidUnstake {
		t.Fatalf("expect invalid unstake, got %v", err)
	}
	// 他人仓位拒绝（F4）
	if _, err := s.Unstake(1, pos.ID, 0); err != ErrNotOwner {
		t.Fatalf("expect not owner, got %v", err)
	}
	// 部分解押：状态保持 active，奖励基数正确缩减
	now = base.AddDate(0, 1, 0) // 先攒一个月奖励
	if _, err := s.Unstake(2, pos.ID, 400); err != nil {
		t.Fatalf("partial unstake: %v", err)
	}
	got, _ := s.store.GetPosition(pos.ID)
	if got.Status != PosActive || !eqAmt(got.Staked, 600, "USDT") {
		t.Fatalf("after partial unstake status=%s staked=%v", got.Status, got.Staked.HumanFloat())
	}
	avail, _, _ := l.Balance(2, "USDT")
	if !eqAmt(avail, 99400, "USDT") {
		t.Fatalf("avail after partial unstake=%v", avail.HumanFloat())
	}
	if !l.IsBalanced() {
		t.Fatal("ledger unbalanced")
	}
}
