package ledger

import (
	"math/big"
	"testing"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// 本文件是战役收尾的「跨服务资金流 e2e 集成测试」。它不依赖 MySQL（Ledger 为纯内存实现，
// MySQL 仅用于可选的幂等指纹持久化），因此默认即可运行，作为整轮 F1–F5 定点化/原子化/幂等
// 硬化的回归保险。
//
// 场景覆盖用户要求的全链路：入金(ReceiveOnChain) → 交易(保证金 Freeze) → 资金费(定点化多腿
// Batch，经 SysFundingPool 净额恒为 0) → 强平瀑布(保险基金没收保证金 + 垫付残损到 SysLiquidationLoss)
// → 结算(PnL 经 Batch 原子入账) → 出金(白名单 + 冷静期 + 链上清算 SettleWithdraw)。
//
// 全程断言三类资金安全不变量：
//  1. 储备金不变量 InventoryMatchesLiability == 0（链上实际持仓(hot+cold)恒等于对用户净负债）。
//  2. 复式记账守恒：同资产全部流水 Delta 之和恒为 0（无凭空铸币/销毁）。
//  3. 用户账户余额永不为负（Available/Frozen/WithdrawFrozen）。
//
// 另含 F1 幂等（重放同 ref 的 Batch 不改变余额）与 F3 原子性（Batch 任一步失败整体回滚）专项断言。

const (
	e2eAsset    = "USDT"
	e2eExchange = int64(1) // 交易所自有金库（链上账户），用于注入保险基金等自有关资本
	e2eUserA    = int64(1001)
	e2eUserB    = int64(1002)
)

// usdt 以人类单位(int)构造 8 位小数的定点 AssetAmount（最小单位整数，杜绝 float 漂移）。
func usdt(human int64) settlement.AssetAmount {
	return settlement.AssetAmountFromInt64(human, 8)
}

// sumDeltas 汇总某资产全部流水的 Delta（带符号），复式记账下应恒为 0。
func sumDeltas(L *Ledger, asset string) settlement.AssetAmount {
	var s settlement.AssetAmount
	for _, e := range L.Log() {
		if e.Asset != asset {
			continue
		}
		s = s.Add(e.Delta)
	}
	return s
}

// assertInvariants 在每个阶段的边界断言资金安全不变量。
func assertInvariants(t *testing.T, L *Ledger, asset string, users ...int64) {
	t.Helper()
	inv := L.InventoryMatchesLiability(asset)
	if !inv.IsZero() {
		t.Fatalf("储备金不变量被破坏 %s: 偏差=%s（hot+cold 应等于对用户净负债）", asset, inv.HumanString())
	}
	if s := sumDeltas(L, asset); !s.IsZero() {
		t.Fatalf("复式记账不守恒 %s: 流水 Delta 之和=%s（应为 0）", asset, s.HumanString())
	}
	for _, u := range users {
		av, fr, ok := L.Balance(u, asset)
		if !ok {
			continue
		}
		if av.Sign() < 0 {
			t.Fatalf("用户 %d 可用余额为负的：%s", u, av.HumanString())
		}
		if fr.Sign() < 0 {
			t.Fatalf("用户 %d 冻结余额为负的：%s", u, fr.HumanString())
		}
		if wf, ok2 := L.WithdrawFrozenBalance(u, asset); ok2 && wf.Sign() < 0 {
			t.Fatalf("用户 %d 提现冻结余额为负的", u)
		}
	}
}

// TestMoneyFlowEndToEnd 全链路资金流场景：入金→交易→资金费→强平→结算→出金。
func TestMoneyFlowEndToEnd(t *testing.T) {
	L := New()

	// —— 阶段 0：入金（链上确认入账，复式记账 Debit 负债账户 + Credit 用户）——
	if err := L.ReceiveOnChain(e2eExchange, e2eAsset, usdt(1_000_000), "genesis-exchange"); err != nil {
		t.Fatalf("genesis exchange deposit: %v", err)
	}
	if err := L.ReceiveOnChain(e2eUserA, e2eAsset, usdt(10_000), "dep-A"); err != nil {
		t.Fatalf("user A deposit: %v", err)
	}
	if err := L.ReceiveOnChain(e2eUserB, e2eAsset, usdt(10_000), "dep-B"); err != nil {
		t.Fatalf("user B deposit: %v", err)
	}
	assertInvariants(t, L, e2eAsset, e2eExchange, e2eUserA, e2eUserB)

	// 交易所自有金库向保险基金注资（自有关资本，经 SysChainClearing 对称记账）。
	if err := L.Transfer(e2eExchange, SysInsurance, e2eAsset, usdt(50_000), "insurance-seed", "seed-ins"); err != nil {
		t.Fatalf("seed insurance: %v", err)
	}
	assertInvariants(t, L, e2eAsset, e2eExchange, e2eUserA, e2eUserB)

	// —— 阶段 1：交易（开仓冻结保证金）——
	if err := L.Freeze(e2eUserA, e2eAsset, usdt(2_000), "open-A"); err != nil {
		t.Fatalf("freeze A margin: %v", err)
	}
	if err := L.Freeze(e2eUserB, e2eAsset, usdt(2_000), "open-B"); err != nil {
		t.Fatalf("freeze B margin: %v", err)
	}
	assertInvariants(t, L, e2eAsset, e2eExchange, e2eUserA, e2eUserB)

	// —— 阶段 2：资金费（多头 A 付空头 B，经 SysFundingPool 中转，净额恒为 0）——
	fundRef := "funding:round-1"
	if err := L.Batch([]Op{
		{Kind: OpTransfer, From: e2eUserA, To: SysFundingPool, Asset: e2eAsset, Amount: usdt(50), Biz: "funding", Ref: fundRef},
		{Kind: OpTransfer, From: SysFundingPool, To: e2eUserB, Asset: e2eAsset, Amount: usdt(50), Biz: "funding", Ref: fundRef},
	}); err != nil {
		t.Fatalf("funding batch: %v", err)
	}
	// 校验：A 减少 50、B 增加 50、资金费池归零。
	aAv, _, _ := L.Balance(e2eUserA, e2eAsset)
	if aAv.Cmp(usdt(7_950)) != 0 {
		t.Fatalf("after funding A available = %s, want 7950", aAv.HumanString())
	}
	bAv, _, _ := L.Balance(e2eUserB, e2eAsset)
	if bAv.Cmp(usdt(8_050)) != 0 {
		t.Fatalf("after funding B available = %s, want 8050", bAv.HumanString())
	}
	poolAv, _, _ := L.Balance(SysFundingPool, e2eAsset)
	if !poolAv.IsZero() {
		t.Fatalf("funding pool should net to 0, got %s", poolAv.HumanString())
	}
	assertInvariants(t, L, e2eAsset, e2eExchange, e2eUserA, e2eUserB)

	// —— 阶段 3：强平瀑布（A 穿仓亏损 5000 > 保证金 2000）——
	// 1) 释放 A 保证金回可用；2) 保险基金没收保证金(+2000)；3) 保险基金垫付残损 3000 到系统损失账户。
	if err := L.Batch([]Op{
		{Kind: OpUnfreeze, User: e2eUserA, Asset: e2eAsset, Amount: usdt(2_000), Ref: "liq-A:unfreeze"},
		{Kind: OpTransfer, From: e2eUserA, To: SysInsurance, Asset: e2eAsset, Amount: usdt(2_000), Biz: "liq_margin", Ref: "liq-A:margin"},
		{Kind: OpTransfer, From: SysInsurance, To: SysLiquidationLoss, Asset: e2eAsset, Amount: usdt(3_000), Biz: "liq_deficit", Ref: "liq-A:deficit"},
	}); err != nil {
		t.Fatalf("liquidation batch: %v", err)
	}
	aAv, aFr, _ := L.Balance(e2eUserA, e2eAsset)
	if aAv.Cmp(usdt(7_950)) != 0 || !aFr.IsZero() {
		t.Fatalf("after liquidation A = avail %s frozen %s, want avail 7950 frozen 0", aAv.HumanString(), aFr.HumanString())
	}
	insAv, _, _ := L.Balance(SysInsurance, e2eAsset)
	// 保险基金：seed 50000 + 没收 2000 - 垫付 3000 = 49000。
	if insAv.Cmp(usdt(49_000)) != 0 {
		t.Fatalf("after liquidation insurance = %s, want 49000", insAv.HumanString())
	}
	lossAv, _, _ := L.Balance(SysLiquidationLoss, e2eAsset)
	if lossAv.Cmp(usdt(3_000)) != 0 {
		t.Fatalf("after liquidation system loss = %s, want 3000", lossAv.HumanString())
	}
	assertInvariants(t, L, e2eAsset, e2eExchange, e2eUserA, e2eUserB)

	// —— 阶段 4：结算（B 平空获利，保证金释放 + PnL 经 Batch 原子入账）——
	if err := L.Batch([]Op{
		{Kind: OpUnfreeze, User: e2eUserB, Asset: e2eAsset, Amount: usdt(2_000), Ref: "close-B:unfreeze"},
		{Kind: OpTransfer, From: SysInsurance, To: e2eUserB, Asset: e2eAsset, Amount: usdt(1_000), Biz: "pnl", Ref: "close-B:pnl"},
	}); err != nil {
		t.Fatalf("settle batch: %v", err)
	}
	bAv2, bFr, _ := L.Balance(e2eUserB, e2eAsset)
	if bAv2.Cmp(usdt(11_050)) != 0 || !bFr.IsZero() {
		t.Fatalf("after settle B = avail %s frozen %s, want avail 11050 frozen 0", bAv2.HumanString(), bFr.HumanString())
	}
	assertInvariants(t, L, e2eAsset, e2eExchange, e2eUserA, e2eUserB)

	// —— 阶段 5：出金（白名单 + 验证冷静期 + 链上清算）——
	L.SetAddressVerifyPeriod(0) // 测试放宽验证冷静期
	L.SetWithdrawHoldPeriod(0)  // 测试放宽提现冷静期
	if _, err := L.AddWithdrawAddress(e2eUserA, e2eAsset, "eth", "0xWHITELISTED", "main"); err != nil {
		t.Fatalf("add withdraw address: %v", err)
	}
	if err := L.ConfirmWithdrawAddress(e2eUserA, e2eAsset, "eth", "0xWHITELISTED"); err != nil {
		t.Fatalf("confirm withdraw address: %v", err)
	}
	holdID, _, err := L.RequestWithdrawHold(e2eUserA, e2eAsset, usdt(1_000), usdt(1), "eth", "0xWHITELISTED")
	if err != nil {
		t.Fatalf("request withdraw hold: %v", err)
	}
	if _, err := L.FinalizeWithdrawHoldForce(holdID); err != nil {
		t.Fatalf("finalize withdraw: %v", err)
	}
	// 校验：A 提现冻结清零、可用扣减净额(1001)、链上负债账户回升、储备金不变量仍成立。
	aAv, _, _ = L.Balance(e2eUserA, e2eAsset)
	if aAv.Cmp(usdt(6_949)) != 0 {
		t.Fatalf("after withdraw A available = %s, want 6949", aAv.HumanString())
	}
	if wf, _ := L.WithdrawFrozenBalance(e2eUserA, e2eAsset); !wf.IsZero() {
		t.Fatalf("after withdraw A withdraw-frozen should be 0, got %s", wf.HumanString())
	}
	assertInvariants(t, L, e2eAsset, e2eExchange, e2eUserA, e2eUserB)
}

// TestMoneyFlowIdempotent F1：重放同 ref 的 Batch（资金费）不改变任何余额。
func TestMoneyFlowIdempotent(t *testing.T) {
	L := New()
	if err := L.ReceiveOnChain(e2eUserA, e2eAsset, usdt(10_000), "dep-A"); err != nil {
		t.Fatal(err)
	}
	if err := L.ReceiveOnChain(e2eUserB, e2eAsset, usdt(10_000), "dep-B"); err != nil {
		t.Fatal(err)
	}
	ref := "funding:idem"
	batch := []Op{
		{Kind: OpTransfer, From: e2eUserA, To: SysFundingPool, Asset: e2eAsset, Amount: usdt(50), Biz: "funding", Ref: ref},
		{Kind: OpTransfer, From: SysFundingPool, To: e2eUserB, Asset: e2eAsset, Amount: usdt(50), Biz: "funding", Ref: ref},
	}
	if err := L.Batch(batch); err != nil {
		t.Fatal(err)
	}
	a1, _, _ := L.Balance(e2eUserA, e2eAsset)
	b1, _, _ := L.Balance(e2eUserB, e2eAsset)
	// 重放同 ref（模拟上层重试/去重失效）：账本层纵深去重应使其为 no-op。
	if err := L.Batch(batch); err != nil {
		t.Fatal(err)
	}
	a2, _, _ := L.Balance(e2eUserA, e2eAsset)
	b2, _, _ := L.Balance(e2eUserB, e2eAsset)
	if a1.Cmp(a2) != 0 || b1.Cmp(b2) != 0 {
		t.Fatalf("idempotency broken: A %s->%s, B %s->%s", a1.HumanString(), a2.HumanString(), b1.HumanString(), b2.HumanString())
	}
	assertInvariants(t, L, e2eAsset, e2eUserA, e2eUserB)
}

// TestMoneyFlowBatchAtomic F3：Batch 任一步失败则整体回滚，不留半成品状态。
func TestMoneyFlowBatchAtomic(t *testing.T) {
	L := New()
	if err := L.ReceiveOnChain(e2eUserB, e2eAsset, usdt(10_000), "dep-B"); err != nil {
		t.Fatal(err)
	}
	if err := L.Freeze(e2eUserB, e2eAsset, usdt(2_000), "open-B"); err != nil {
		t.Fatal(err)
	}
	bAvBefore, bFrBefore, _ := L.Balance(e2eUserB, e2eAsset)

	// 一个合法冻结 + 一个必然失败（超出可用）的冻结：整体应回滚到执行前。
	bad := []Op{
		{Kind: OpFreeze, User: e2eUserB, Asset: e2eAsset, Amount: usdt(10), Ref: "atomic-good"},
		{Kind: OpFreeze, User: e2eUserB, Asset: e2eAsset, Amount: usdt(9_999_999), Ref: "atomic-bad"},
	}
	if err := L.Batch(bad); err == nil {
		t.Fatal("expected Batch to fail on insufficient balance, got nil")
	}
	bAvAfter, bFrAfter, _ := L.Balance(e2eUserB, e2eAsset)
	if bAvAfter.Cmp(bAvBefore) != 0 || bFrAfter.Cmp(bFrBefore) != 0 {
		t.Fatalf("Batch rollback failed: frozen %s->%s (want %s), available %s->%s (want %s)",
			bFrBefore.HumanString(), bFrAfter.HumanString(), bFrBefore.HumanString(),
			bAvBefore.HumanString(), bAvAfter.HumanString(), bAvBefore.HumanString())
	}
	assertInvariants(t, L, e2eAsset, e2eUserB)
}

// 确保 math/big 被引用（避免未使用导入告警；部分构建路径下用到）。
var _ = big.NewInt
