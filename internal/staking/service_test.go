package staking

import (
	"math/big"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

func newTestService() (*Service, *ledger.Ledger, Store) {
	store := NewMemStore()
	l := ledger.New()
	svc := NewService(store, l, NewMockBackend(), Config{AccrueInterval: time.Hour}, nil)
	return svc, l, store
}

// TestStakingLifecycle 验证「质押锁定 -> 奖励归集 -> 解质押 -> 释放」完整资金闭环，
// 以及 F2 定点 / F3 原子 / 账本复式守恒。
func TestStakingLifecycle(t *testing.T) {
	svc, l, store := newTestService()
	dec := settlement.AssetDecimalsByName("ETH")

	// 给用户 1 充值 10 ETH。
	seed := settlement.AssetAmountFromFloat(10, dec)
	if err := l.ReceiveOnChain(1, "ETH", seed, "seed"); err != nil {
		t.Fatal(err)
	}

	// 管理员创建在售产品（绕过 handler 直接落库）。
	p := &StakingProduct{
		Name: "ETH", Chain: "eth", Validator: "v", ContractAddr: "c",
		Asset: "ETH", MinAmount: settlement.NewAssetAmount(big.NewInt(0), dec), Status: ProductActive,
	}
	if err := store.CreateProduct(p); err != nil {
		t.Fatal(err)
	}

	// 1) 质押：锁本金。
	stakeAmt := settlement.AssetAmountFromFloat(5, dec)
	d, err := svc.Subscribe(1, p.ID, stakeAmt)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if d.Status != DelegationActive {
		t.Fatalf("status=%s, want active", d.Status)
	}
	if d.TxHash == "" {
		t.Fatal("expected tx hash")
	}
	if av, _, _ := l.Balance(1, "ETH"); av.Cmp(settlement.AssetAmountFromFloat(5, dec)) != 0 {
		t.Fatalf("user balance after stake = %s, want 5", av.HumanString())
	}
	if sk, _, _ := l.Balance(ledger.SysStaking, "ETH"); sk.Cmp(stakeAmt) != 0 {
		t.Fatalf("SysStaking = %s, want 5", sk.HumanString())
	}

	// 2) 奖励归集（Mock 后端返回固定待领取奖励）。
	if _, err := svc.Accrue(time.Now()); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	rews, _ := store.ListRewardsByDelegation(d.ID)
	if len(rews) != 1 {
		t.Fatalf("rewards=%d, want 1", len(rews))
	}
	if sr, _, _ := l.Balance(ledger.SysStakingReward, "ETH"); sr.Sign() <= 0 {
		t.Fatalf("SysStakingReward should be positive, got %s", sr.HumanString())
	}

	// 3) 解质押：标记 unbonding。
	if _, err := svc.Unbond(1, d.ID); err != nil {
		t.Fatalf("unbond: %v", err)
	}
	if d2, _ := store.GetDelegation(d.ID); d2.Status != DelegationUnbonding {
		t.Fatalf("status=%s, want unbonding", d2.Status)
	}
	// F1 幂等：终态再解质押应报错。
	if _, err := svc.Unbond(1, d.ID); err == nil {
		t.Fatal("expected error on repeated unbond")
	}

	// 4) 释放：链上确认达标后本金+奖励原子释放给用户。
	if _, err := svc.Release(1, d.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if d3, _ := store.GetDelegation(d.ID); d3.Status != DelegationUnbonded {
		t.Fatalf("status=%s, want unbonded", d3.Status)
	}
	want := seed.Add(rews[0].Amount) // 10 + 奖励
	if av, _, _ := l.Balance(1, "ETH"); av.Cmp(want) != 0 {
		t.Fatalf("user balance after release = %s, want %s", av.HumanString(), want.HumanString())
	}
}

// TestSubscribeUniqueRefNoDoubleDedupe 验证 F1 修复：同一用户对同一产品多次质押，
// 每笔委托的本金锁定 ref 锚定各自委托 ID（stake_lock:<id>），互不复用，故每笔都真实锁
// 定本金、不漏锁。早期版本用 stake:<user>:<product> 作 ref，第二次质押会被账本幂等去重
// 跳过、导致 SysStaking 少记本金——此处断言两笔均锁定。
func TestSubscribeUniqueRefNoDoubleDedupe(t *testing.T) {
	svc, l, store := newTestService()
	dec := settlement.AssetDecimalsByName("ETH")
	p := &StakingProduct{
		Name: "ETH", Chain: "eth", Validator: "v", ContractAddr: "c",
		Asset: "ETH", MinAmount: settlement.NewAssetAmount(big.NewInt(0), dec), Status: ProductActive,
	}
	if err := store.CreateProduct(p); err != nil {
		t.Fatal(err)
	}
	seed := settlement.AssetAmountFromFloat(20, dec)
	if err := l.ReceiveOnChain(1, "ETH", seed, "seed"); err != nil {
		t.Fatal(err)
	}
	amt := settlement.AssetAmountFromFloat(5, dec)
	d1, err := svc.Subscribe(1, p.ID, amt)
	if err != nil {
		t.Fatalf("subscribe1: %v", err)
	}
	d2, err := svc.Subscribe(1, p.ID, amt)
	if err != nil {
		t.Fatalf("subscribe2: %v", err)
	}
	if d1.ID == d2.ID {
		t.Fatal("expected distinct delegation IDs")
	}
	if sk, _, _ := l.Balance(ledger.SysStaking, "ETH"); sk.Cmp(settlement.AssetAmountFromFloat(10, dec)) != 0 {
		t.Fatalf("SysStaking = %s, want 10 (two locks)", sk.HumanString())
	}
	if av, _, _ := l.Balance(1, "ETH"); av.Cmp(settlement.AssetAmountFromFloat(10, dec)) != 0 {
		t.Fatalf("user balance = %s, want 10", av.HumanString())
	}
}

// TestStakingReconcile 验证 F3 业务对账：完整生命周期后，在押本金与已归集奖励应分别与
// SysStaking / SysStakingReward 账本账户逐笔对平（偏差全为 0）。
func TestStakingReconcile(t *testing.T) {
	svc, l, store := newTestService()
	dec := settlement.AssetDecimalsByName("ETH")
	p := &StakingProduct{
		Name: "ETH", Chain: "eth", Validator: "v", ContractAddr: "c",
		Asset: "ETH", MinAmount: settlement.NewAssetAmount(big.NewInt(0), dec), Status: ProductActive,
	}
	if err := store.CreateProduct(p); err != nil {
		t.Fatal(err)
	}
	if err := l.ReceiveOnChain(1, "ETH", settlement.AssetAmountFromFloat(10, dec), "seed"); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Subscribe(1, p.ID, settlement.AssetAmountFromFloat(5, dec))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := svc.Accrue(time.Now()); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if _, err := svc.Unbond(1, d.ID); err != nil {
		t.Fatalf("unbond: %v", err)
	}
	if _, err := svc.Release(1, d.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	// 释放后委托已离开在押集合，账本应完全回零 -> 对账偏差为 0。
	dev := svc.Reconcile()
	for asset, v := range dev {
		if v.Sign() != 0 {
			t.Fatalf("reconcile deviation on %s = %s, want 0", asset, v.HumanString())
		}
	}
	if !l.IsBalanced() {
		t.Fatal("ledger not globally balanced after full lifecycle")
	}
}

// TestSubscribeBelowMin 验证 F5 边界：低于起质押额被拒。
func TestSubscribeBelowMin(t *testing.T) {
	svc, _, store := newTestService()
	dec := settlement.AssetDecimalsByName("ETH")
	p := &StakingProduct{
		Name: "ETH", Chain: "eth", Asset: "ETH",
		MinAmount: settlement.AssetAmountFromFloat(1, dec), Status: ProductActive,
	}
	if err := store.CreateProduct(p); err != nil {
		t.Fatal(err)
	}
	// 0.5 < 1 起质押额
	if _, err := svc.Subscribe(1, p.ID, settlement.AssetAmountFromFloat(0.5, dec)); err != ErrBelowMinAmount {
		t.Fatalf("want ErrBelowMinAmount, got %v", err)
	}
}
