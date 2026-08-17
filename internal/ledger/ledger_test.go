package ledger_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// amt 把人类单位浮点按资产标准小数位包装为 AssetAmount（测试边界用）。
func amt(asset string, human float64) settlement.AssetAmount {
	return settlement.AssetAmountFromFloat(human, settlement.AssetDecimalsByName(asset))
}

// eqAmt 比较实际 AssetAmount 与人类单位期望值（按 actual.Decimals 对齐）。
func eqAmt(actual settlement.AssetAmount, wantHuman float64) bool {
	return actual.Cmp(settlement.AssetAmountFromFloat(wantHuman, actual.Decimals)) == 0
}

// assertBalance 检查可用/冻结余额。账户不存在且期望均为 0 时视为通过
// （例如从未动用的坏账账户），其余情况账户不存在即报错。
func assertBalance(t *testing.T, l *ledger.Ledger, uid int64, asset string, avail, frozen float64) {
	t.Helper()
	a, f, ok := l.Balance(uid, asset)
	if !ok {
		if eqAmt(a, avail) && eqAmt(f, frozen) {
			return
		}
		t.Fatalf("account %d:%s not found (want avail %.8f frozen %.8f)", uid, asset, avail, frozen)
	}
	if !eqAmt(a, avail) || !eqAmt(f, frozen) {
		t.Fatalf("user %d balance = avail %s frozen %s, want avail %.8f frozen %.8f",
			uid, a.HumanString(), f.HumanString(), avail, frozen)
	}
}

// approx 浮点容差比较（保留以便旧逻辑 / 纯 float 比较复用）。
func approx(a, b float64) int {
	const eps = 1e-6
	d := a - b
	if d > eps {
		return 1
	}
	if d < -eps {
		return -1
	}
	return 0
}

// TestFreezeUnfreeze 开仓冻结 / 平仓释放。
func TestFreezeUnfreeze(t *testing.T) {
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 10000, 0)

	// 开仓冻结 5000 保证金
	if err := l.Freeze(1, "USDT", amt("USDT", 5000)); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 5000, 5000)

	// 余额不足时拒绝
	if err := l.Freeze(1, "USDT", amt("USDT", 99999)); err == nil {
		t.Fatal("expected insufficient balance error")
	}

	// 平仓释放
	if err := l.Unfreeze(1, "USDT", amt("USDT", 5000)); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 10000, 0)
}

// TestFreezeIdempotentByRef 验证：传入非空 ref 时，相同指纹的重复 Freeze 为 no-op（不二次扣减可用/不二次增加冻结）。
func TestFreezeIdempotentByRef(t *testing.T) {
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	amount := amt("USDT", 5000)
	// 第一次冻结生效
	if err := l.Freeze(1, "USDT", amount, "ref-1"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 5000, 5000)
	// 相同 ref 重复提交：应为 no-op
	if err := l.Freeze(1, "USDT", amount, "ref-1"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 5000, 5000) // 余额未变
}

// TestUnfreezeIdempotentByRef 验证：传入非空 ref 时，相同指纹的重复 Unfreeze 为 no-op。
func TestUnfreezeIdempotentByRef(t *testing.T) {
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	amount := amt("USDT", 5000)
	if err := l.Freeze(1, "USDT", amount, "freeze-ref"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 5000, 5000)
	// 第一次解冻生效
	if err := l.Unfreeze(1, "USDT", amount, "ref-2"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 10000, 0)
	// 相同 ref 重复提交：应为 no-op（不会把可用余额再凭空增加）
	if err := l.Unfreeze(1, "USDT", amount, "ref-2"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 10000, 0) // 余额未变
}

// TestFreezeWithoutRefNoDedupe 回归保护：不传 ref 时保持原语义，每次调用均生效。
func TestFreezeWithoutRefNoDedupe(t *testing.T) {
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	amount := amt("USDT", 5000)
	if err := l.Freeze(1, "USDT", amount); err != nil {
		t.Fatal(err)
	}
	// 无 ref 重复调用：仍生效（可用再扣 5000、冻结再增 5000）
	if err := l.Freeze(1, "USDT", amount); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 0, 10000) // 两次都生效
}

// TestFreezeDistinctRefsApply 验证：含 amount 的指纹确保「同 ref 不同金额」与「同金额不同 ref」均不被误去重。
func TestFreezeDistinctRefsApply(t *testing.T) {
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 100000), "seed"); err != nil {
		t.Fatal(err)
	}
	// 同 ref "x"，不同金额：两笔都应生效（指纹因 amount 不同而不同）
	if err := l.Freeze(1, "USDT", amt("USDT", 10), "x"); err != nil {
		t.Fatal(err)
	}
	if err := l.Freeze(1, "USDT", amt("USDT", 20), "x"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 100000-30, 30)
	// 同金额，不同 ref "a"/"b"：两笔都应生效（指纹因 ref 不同而不同）
	if err := l.Freeze(1, "USDT", amt("USDT", 5), "a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Freeze(1, "USDT", amt("USDT", 5), "b"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 100000-40, 40)
}

// TestFreezeWithdrawIdempotentByRef 覆盖 FreezeWithdraw/UnfreezeWithdraw 入口的幂等。
func TestFreezeWithdrawIdempotentByRef(t *testing.T) {
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	amount := amt("USDT", 5000)
	// 提现冻结（带 ref），重复提交应为 no-op
	if err := l.FreezeWithdraw(1, "USDT", amount, "wd-ref"); err != nil {
		t.Fatal(err)
	}
	if err := l.FreezeWithdraw(1, "USDT", amount, "wd-ref"); err != nil {
		t.Fatal(err)
	}
	// 注意：Balance 的第二个返回值是持仓保证金冻结(Frozen)，提现冻结需用 WithdrawFrozenBalance 读取。
	_, avail, _ := l.Balance(1, "USDT")
	wf, _ := l.WithdrawFrozenBalance(1, "USDT")
	if !eqAmt(wf, 5000) {
		t.Fatalf("withdraw frozen = %s, want 5000 (avail=%s)", wf.HumanString(), avail.HumanString())
	}
	// 退回（带不同 ref），重复提交应为 no-op
	if err := l.UnfreezeWithdraw(1, "USDT", amount, "unwd-ref"); err != nil {
		t.Fatal(err)
	}
	if err := l.UnfreezeWithdraw(1, "USDT", amount, "unwd-ref"); err != nil {
		t.Fatal(err)
	}
	avail2, _, _ := l.Balance(1, "USDT")
	wf2, _ := l.WithdrawFrozenBalance(1, "USDT")
	if !eqAmt(wf2, 0) || !eqAmt(avail2, 10000) {
		t.Fatalf("after unwind: avail=%s wf=%s, want avail=10000 wf=0", avail2.HumanString(), wf2.HumanString())
	}
}

// TestFundingClosedLoop 资金费多空转账闭环：净额恒为零。
func TestFundingClosedLoop(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	// 多头 user1、空头 user2，各充值 10000
	l.Deposit(1, asset, amt(asset, 10000), "seed")
	l.Deposit(2, asset, amt(asset, 10000), "seed")

	// 模拟一轮资金结算：正费率 0.01%，双方名义价值 50000
	rate := 0.0001
	notional := 50000.0
	payments := []futures.FundingPayment{
		{UserID: 1, Side: futures.Long, Notional: notional, Payment: -notional * rate}, // 多头付
		{UserID: 2, Side: futures.Short, Notional: notional, Payment: notional * rate}, // 空头收
	}

	// 应用资金费到钱包：通过资金费中转池，保证借贷恒等
	ref := "funding:BTC_USDT_PERP:1"
	for _, p := range payments {
		switch {
		case p.Payment < 0:
			if err := l.Transfer(p.UserID, ledger.SysFundingPool, asset, amt(asset, -p.Payment), "funding", ref); err != nil {
				t.Fatal(err)
			}
		case p.Payment > 0:
			if err := l.Transfer(ledger.SysFundingPool, p.UserID, asset, amt(asset, p.Payment), "funding", ref); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 多头应少 5，空头应多 5
	assertBalance(t, l, 1, asset, 9995, 0)
	assertBalance(t, l, 2, asset, 10005, 0)
	// 中转池净额应为 0
	poolAvail, _, _ := l.Balance(ledger.SysFundingPool, asset)
	if !eqAmt(poolAvail, 0) {
		t.Fatalf("funding pool should be 0, got %s", poolAvail.HumanString())
	}
}

// TestLiquidationForfeit 强平没收保证金入保险基金。
func TestLiquidationForfeit(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	l.Deposit(1, asset, amt(asset, 10000), "seed")
	l.Freeze(1, asset, amt(asset, 5000)) // 开仓锁 5000

	// 强平：没收冻结保证金到保险基金，账户清零（演示简化）
	margin := 5000.0
	if err := l.Unfreeze(1, asset, amt(asset, margin)); err != nil {
		t.Fatal(err)
	}
	if err := l.DebitAvailable(1, asset, amt(asset, margin), "liquidation", "liq:1"); err != nil {
		t.Fatal(err)
	}
	if err := l.CreditAvailable(ledger.SysInsurance, asset, amt(asset, margin), "liquidation", "liq:1"); err != nil {
		t.Fatal(err)
	}

	assertBalance(t, l, 1, asset, 5000, 0)                   // 剩余可用
	assertBalance(t, l, ledger.SysInsurance, asset, 5000, 0) // 保险基金 +5000

	// 强平业务借贷恒等：用户1 -5000 与 保险基金 +5000 相抵
	var liqSum settlement.AssetAmount
	for _, e := range l.Log() {
		if e.BizType == "liquidation" {
			liqSum = liqSum.Add(e.Delta)
		}
	}
	if liqSum.Sign() != 0 {
		t.Fatalf("liquidation debit/credit must net to 0, got %s", liqSum.HumanString())
	}
}

// TestReceiveOnChain 链上充值入账：用户可用 +amount，链上清算负债账户 -amount，
// chain_deposit 业务借贷恒等（净额 0）。
func TestReceiveOnChain(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	tx := "0xabc123"
	if err := l.ReceiveOnChain(7777, asset, amt(asset, 5000), tx); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 7777, asset, 5000, 0)                  // 用户入账
	assertBalance(t, l, ledger.SysChainClearing, asset, -5000, 0) // 清算负债 -5000

	var sum settlement.AssetAmount
	for _, e := range l.Log() {
		if e.BizType == "chain_deposit" {
			sum = sum.Add(e.Delta)
		}
	}
	if sum.Sign() != 0 {
		t.Fatalf("chain_deposit debit/credit must net to 0, got %s", sum.HumanString())
	}
}

// TestSettleWithdraw 链上提现清算：提现额+手续费从冻结余额原子划出，
// 贷记链上清算负债账户（余额回升），chain_withdraw 业务借贷恒等（净额 0）。
func TestSettleWithdraw(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7777)
	l.Deposit(uid, asset, amt(asset, 10000), "seed")
	// 提交提现：冻结 1000 + 2 手续费（走独立的提现冻结 WithdrawFrozen）
	if err := l.FreezeWithdraw(uid, asset, amt(asset, 1002)); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8998, 0) // 可用减少，持仓冻结不变
	wf, _ := l.WithdrawFrozenBalance(uid, asset)
	if !eqAmt(wf, 1002) {
		t.Fatalf("withdraw frozen expect 1002, got %s", wf.HumanString())
	}

	// 链上确认达标，结算划出
	if err := l.SettleWithdraw(uid, asset, amt(asset, 1000), amt(asset, 2), "0xwtx1"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8998, 0) // 提现冻结清零，可用不变（钱已离开系统）
	wf, _ = l.WithdrawFrozenBalance(uid, asset)
	if !eqAmt(wf, 0) {
		t.Fatalf("withdraw frozen expect 0 after settle, got %s", wf.HumanString())
	}
	// SysChainClearing 从 0 回升到 +1002（对用户负债减少）
	assertBalance(t, l, ledger.SysChainClearing, asset, 1002, 0)

	var sum settlement.AssetAmount
	for _, e := range l.Log() {
		if e.BizType == "chain_withdraw" {
			sum = sum.Add(e.Delta)
		}
	}
	if sum.Sign() != 0 {
		t.Fatalf("chain_withdraw debit/credit must net to 0, got %s", sum.HumanString())
	}
}

// TestSettleWithdrawInsufficientFrozen 冻结不足时拒绝结算（防资损）。
func TestSettleWithdrawInsufficientFrozen(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7778)
	l.Deposit(uid, asset, amt(asset, 100), "seed")
	// 未冻结就结算，应报错
	if err := l.SettleWithdraw(uid, asset, amt(asset, 1000), amt(asset, 2), "0xwtx2"); err == nil {
		t.Fatal("expected insufficient frozen error")
	}
}

// TestWithdrawDepositClosedLoop 出入金闭环：先充值再提现，SysChainClearing 回到 0。
func TestWithdrawDepositClosedLoop(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7779)
	// 充值 5000 -> 清算负债 -5000
	l.ReceiveOnChain(uid, asset, amt(asset, 5000), "0xin")
	// 提现 5000（含费 0） -> 提现冻结 5000，结算后清算负债回到 0
	l.FreezeWithdraw(uid, asset, amt(asset, 5000))
	if err := l.SettleWithdraw(uid, asset, amt(asset, 5000), amt(asset, 0), "0xout"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 0, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, 0, 0) // 出入金对冲，负债归零
}

// TestReverseOnChain 孤块回滚：充值入账后再回拨，用户余额与清算负债回到 0，
// chain_rollback 业务借贷恒等（净额 0）。与 ReceiveOnChain 互逆。
func TestReverseOnChain(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7780)
	tx := "0xin2"
	l.ReceiveOnChain(uid, asset, amt(asset, 4000), tx)
	assertBalance(t, l, uid, asset, 4000, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, -4000, 0)

	// 孤块丢弃，回拨（全额可用，无坏账）
	badDebt, err := l.ReverseOnChain(uid, asset, amt(asset, 4000), tx)
	if err != nil {
		t.Fatal(err)
	}
	if badDebt.Sign() != 0 {
		t.Fatalf("expected zero bad debt, got %s", badDebt.HumanString())
	}
	assertBalance(t, l, uid, asset, 0, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, 0, 0) // 负债回升归零
	assertBalance(t, l, ledger.SysBadDebt, asset, 0, 0)       // 无坏账

	var sum settlement.AssetAmount
	for _, e := range l.Log() {
		if e.BizType == "chain_rollback" {
			sum = sum.Add(e.Delta)
		}
	}
	if sum.Sign() != 0 {
		t.Fatalf("chain_rollback debit/credit must net to 0, got %s", sum.HumanString())
	}
}

// TestReverseOnChainBadDebt 充值回滚坏账风控：用户已动用部分充值资金，回滚时可用不足，
// 交易所垫付差额记入 SysBadDebt（余额转负），借贷恒等保持不变。
func TestReverseOnChainBadDebt(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7782)
	tx := "0xinBD"
	// 充值 5000 并提现动用 3000（提现冻结 3000 -> 结算划出）
	l.ReceiveOnChain(uid, asset, amt(asset, 5000), tx)
	l.FreezeWithdraw(uid, asset, amt(asset, 3000))
	if err := l.SettleWithdraw(uid, asset, amt(asset, 3000), amt(asset, 0), "0xoutBD"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 2000, 0)            // 仅剩 2000 可用
	assertBalance(t, l, ledger.SysChainClearing, asset, -2000, 0)

	// 充值被孤块丢弃：只能回拨 2000，剩余 3000 为坏账
	badDebt, err := l.ReverseOnChain(uid, asset, amt(asset, 5000), tx)
	if err != nil {
		t.Fatal(err)
	}
	if !eqAmt(badDebt, 3000) {
		t.Fatalf("expected bad debt 3000, got %s", badDebt.HumanString())
	}
	assertBalance(t, l, uid, asset, 0, 0)                   // 可用扣到 0
	assertBalance(t, l, ledger.SysChainClearing, asset, 3000, 0)  // 负债完全回升
	assertBalance(t, l, ledger.SysBadDebt, asset, -3000, 0)       // 交易所垫付坏账

	// 全局借贷恒等：全部账户净额之和应为 0
	var total settlement.AssetAmount
	for _, e := range l.Log() {
		total = total.Add(e.Delta)
	}
	if total.Sign() != 0 {
		t.Fatalf("global ledger must net to 0, got %s", total.HumanString())
	}
}

// TestReverseWithdraw 提现孤块回滚：提现已清结算划出后若被重组丢弃，ReverseWithdraw
// 把资金从链上负债账户回拨到用户冻结，与 SettleWithdraw 互逆，借贷恒等。
func TestReverseWithdraw(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7783)
	l.Deposit(uid, asset, amt(asset, 10000), "seed")
	l.FreezeWithdraw(uid, asset, amt(asset, 1002)) // 提现 1000 + 费 2（提现冻结）
	// 清结算划出
	if err := l.SettleWithdraw(uid, asset, amt(asset, 1000), amt(asset, 2), "0xwtxR"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8998, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, 1002, 0)
	wf, _ := l.WithdrawFrozenBalance(uid, asset)
	if !eqAmt(wf, 0) {
		t.Fatalf("withdraw frozen expect 0 after settle, got %s", wf.HumanString())
	}

	// 提现被孤块重组：回拨到提现冻结（不影响持仓保证金 Frozen）
	if err := l.ReverseWithdraw(uid, asset, amt(asset, 1000), amt(asset, 2), "0xwtxR"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8998, 0)          // 持仓冻结不变
	wf, _ = l.WithdrawFrozenBalance(uid, asset)
	if !eqAmt(wf, 1002) {
		t.Fatalf("withdraw frozen expect 1002 after revert, got %s", wf.HumanString())
	}
	assertBalance(t, l, ledger.SysChainClearing, asset, 0, 0) // 负债回到划出前

	// 退回可用（演示闭环）
	if err := l.UnfreezeWithdraw(uid, asset, amt(asset, 1002)); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 10000, 0) // 用户余额完全恢复

	var sum settlement.AssetAmount
	for _, e := range l.Log() {
		if e.BizType == "chain_withdraw_revert" {
			sum = sum.Add(e.Delta)
		}
	}
	if sum.Sign() != 0 {
		t.Fatalf("chain_withdraw_revert debit/credit must net to 0, got %s", sum.HumanString())
	}
}

// TestDepositWithdrawReorgLoop 完整链上生命周期：充值 -> 提现 -> 充值的孤块回滚，
// 各阶段 SysChainClearing 守恒。
func TestDepositWithdrawReorgLoop(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7781)
	// 充值 5000
	l.ReceiveOnChain(uid, asset, amt(asset, 5000), "0xin3")
	assertBalance(t, l, ledger.SysChainClearing, asset, -5000, 0)
	// 提现 3000（含费 0）
	l.FreezeWithdraw(uid, asset, amt(asset, 3000))
	if err := l.SettleWithdraw(uid, asset, amt(asset, 3000), amt(asset, 0), "0xout3"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 2000, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, -2000, 0) // 负债随提现回升
	// 充值被孤块回滚：用户仅剩 2000 可用，3000 转坏账垫付
	badDebt, _ := l.ReverseOnChain(uid, asset, amt(asset, 5000), "0xin3")
	assertBalance(t, l, uid, asset, 0, 0)                    // 可用扣到 0
	assertBalance(t, l, ledger.SysChainClearing, asset, 3000, 0)  // 负债回升
	assertBalance(t, l, ledger.SysBadDebt, asset, -3000, 0)       // 坏账垫付
	if !eqAmt(badDebt, 3000) {
		t.Fatalf("expected bad debt 3000, got %s", badDebt.HumanString())
	}
}

// TestBadDebtRecovery 坏账自动回收：充值被孤块回滚产生坏账后，用户后续充值会优先冲抵
// 交易所垫付坏账，剩余才入可用；坏账清零后 SysBadDebt 归零，借贷恒等。
func TestBadDebtRecovery(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7784)
	// 充值 5000 -> 提现动用 3000 -> 回滚产生坏账 3000
	l.ReceiveOnChain(uid, asset, amt(asset, 5000), "0xinR")
	l.FreezeWithdraw(uid, asset, amt(asset, 3000))
	if err := l.SettleWithdraw(uid, asset, amt(asset, 3000), amt(asset, 0), "0xoutR"); err != nil {
		t.Fatal(err)
	}
	l.ReverseOnChain(uid, asset, amt(asset, 5000), "0xinR")
	assertBalance(t, l, ledger.SysBadDebt, asset, -3000, 0)
	if !eqAmt(l.BadDebtTotal(asset), 3000) {
		t.Fatalf("bad debt total should be 3000, got %s", l.BadDebtTotal(asset).HumanString())
	}

	// 后续充值 1000：全额冲抵坏账，用户可用仍为 0，坏账剩 2000
	if err := l.ReceiveOnChain(uid, asset, amt(asset, 1000), "0xinR2"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 0, 0)
	assertBalance(t, l, ledger.SysBadDebt, asset, -2000, 0)
	if !eqAmt(l.BadDebtTotal(asset), 2000) {
		t.Fatalf("bad debt total should be 2000, got %s", l.BadDebtTotal(asset).HumanString())
	}

	// 再充值 2000：冲抵剩余 2000，用户可用 0，坏账归零
	if err := l.ReceiveOnChain(uid, asset, amt(asset, 2000), "0xinR3"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 0, 0)
	assertBalance(t, l, ledger.SysBadDebt, asset, 0, 0)
	if !eqAmt(l.BadDebtTotal(asset), 0) {
		t.Fatalf("bad debt should be cleared, got %s", l.BadDebtTotal(asset).HumanString())
	}
}

// TestRepayBadDebt 主动补缴：用户用可用余额手动冲抵坏账，SysBadDebt 回升、用户可用减少，
// 借贷恒等；无坏账或余额不足时拒绝。
func TestRepayBadDebt(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7786)

	// 标准流程制造坏账 5000
	l.ReceiveOnChain(uid, asset, amt(asset, 5000), "0xA")
	l.FreezeWithdraw(uid, asset, amt(asset, 5000))
	l.SettleWithdraw(uid, asset, amt(asset, 5000), amt(asset, 0), "0xB")
	_, _ = l.ReverseOnChain(uid, asset, amt(asset, 5000), "0xA") // 坏账 5000
	assertBalance(t, l, ledger.SysBadDebt, asset, -5000, 0)

	// 余额不足负路径：此时用户可用为 0
	if err := l.RepayBadDebt(uid, asset, amt(asset, 100), "x"); err == nil {
		t.Fatal("repay with insufficient balance should fail")
	}
	// 无坏账用户负路径
	if err := l.RepayBadDebt(7787, asset, amt(asset, 100), "x"); err == nil {
		t.Fatal("repay with no bad debt should fail")
	}

	// 注入可用资金（Deposit 非链上充值，不触发自动回收）
	l.Deposit(uid, asset, amt(asset, 5000), "seed")
	assertBalance(t, l, uid, asset, 5000, 0)
	// 正向：补缴 2000 -> 坏账 3000，可用 3000
	if err := l.RepayBadDebt(uid, asset, amt(asset, 2000), "repay:7786"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, ledger.SysBadDebt, asset, -3000, 0)
	assertBalance(t, l, uid, asset, 3000, 0)
	// 再补缴 3000 -> 坏账清零，可用 0
	if err := l.RepayBadDebt(uid, asset, amt(asset, 3000), "repay:7786"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, ledger.SysBadDebt, asset, 0, 0)
	assertBalance(t, l, uid, asset, 0, 0)
}

// TestOutflowRestriction 坏账强制限提：产生坏账即限制出金，回收/补缴清零后自动解除。
func TestOutflowRestriction(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7788)
	// 制造坏账 3000
	l.ReceiveOnChain(uid, asset, amt(asset, 5000), "0xA")
	l.FreezeWithdraw(uid, asset, amt(asset, 3000))
	l.SettleWithdraw(uid, asset, amt(asset, 3000), amt(asset, 0), "0xB")
	_, _ = l.ReverseOnChain(uid, asset, amt(asset, 5000), "0xA")
	if !l.IsOutflowRestricted(uid, asset) {
		t.Fatal("user with bad debt should be outflow restricted")
	}
	// 充值回收 1000：坏账剩 2000，仍受限
	l.ReceiveOnChain(uid, asset, amt(asset, 1000), "0xC")
	if !l.IsOutflowRestricted(uid, asset) {
		t.Fatal("still bad debt, should remain restricted")
	}
	// 再充值 2000：坏账清零，解除限制
	l.ReceiveOnChain(uid, asset, amt(asset, 2000), "0xD")
	if l.IsOutflowRestricted(uid, asset) {
		t.Fatal("bad debt cleared, restriction should be lifted")
	}

	// 另一路径：造坏账后主动补缴清零也解除限制
	l2 := ledger.New()
	l2.ReceiveOnChain(uid, asset, amt(asset, 5000), "0xA")
	l2.FreezeWithdraw(uid, asset, amt(asset, 5000))
	l2.SettleWithdraw(uid, asset, amt(asset, 5000), amt(asset, 0), "0xB")
	_, _ = l2.ReverseOnChain(uid, asset, amt(asset, 5000), "0xA")
	if !l2.IsOutflowRestricted(uid, asset) {
		t.Fatal("bad debt should restrict")
	}
	l2.Deposit(uid, asset, amt(asset, 5000), "seed")
	if err := l2.RepayBadDebt(uid, asset, amt(asset, 5000), "repay"); err != nil {
		t.Fatal(err)
	}
	if l2.IsOutflowRestricted(uid, asset) {
		t.Fatal("repay cleared, restriction should be lifted")
	}
}

// TestWithdrawFrozenSeparateFromPositionFrozen 验证两类冻结互斥独立：持仓保证金冻结
// (Frozen) 与提现冻结 (WithdrawFrozen) 由不同接口操作，提现结算/回滚只动 WithdrawFrozen，
// 绝不触碰持仓保证金，杜绝"提现结算误扣保证金"或"回滚提现冲掉开仓保证金"的资损路径。
func TestWithdrawFrozenSeparateFromPositionFrozen(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(9099)
	l.Deposit(uid, asset, amt(asset, 10000), "seed")

	// 开仓锁定保证金 2000（持仓冻结）
	if err := l.Freeze(uid, asset, amt(asset, 2000)); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8000, 2000) // available=8000, 持仓冻结=2000

	// 提交提现冻结 1000（提现冻结，独立于持仓冻结）
	if err := l.FreezeWithdraw(uid, asset, amt(asset, 1000)); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 7000, 2000) // available=7000, 持仓冻结仍为 2000（未被提现冻结影响）
	wf, _ := l.WithdrawFrozenBalance(uid, asset)
	if !eqAmt(wf, 1000) {
		t.Fatalf("withdraw frozen expect 1000, got %s", wf.HumanString())
	}

	// 提现结算划出：只扣提现冻结，持仓保证金必须保持不变
	if err := l.SettleWithdraw(uid, asset, amt(asset, 1000), amt(asset, 0), "0xw"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 7000, 2000) // available/持仓冻结都不因提现改变
	wf, _ = l.WithdrawFrozenBalance(uid, asset)
	if !eqAmt(wf, 0) {
		t.Fatalf("withdraw frozen expect 0 after settle, got %s", wf.HumanString())
	}

	// 持仓保证金释放（平仓）：只动 Frozen，不影响提现冻结（此时已为 0）
	if err := l.Unfreeze(uid, asset, amt(asset, 2000)); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 9000, 0)
	wf, _ = l.WithdrawFrozenBalance(uid, asset)
	if !eqAmt(wf, 0) {
		t.Fatalf("withdraw frozen must stay 0, got %s", wf.HumanString())
	}
}

// TestSocializeBadDebt 验证社会化分摊回收：坏账产生后，先由保险基金冲减，剩余向非受限
// 盈利用户按可用余额占比分摊；分摊后坏账清零并解除出金限制。
func TestSocializeBadDebt(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	debtor := int64(9001) // 产生坏账的用户（受限，不参与分摊）
	alice := int64(9002)  // 盈利用户，余额 4000
	bob := int64(9003)    // 盈利用户，余额 6000

	// 制造坏账 1000（debtor 充值并提现后孤块回滚）
	l.ReceiveOnChain(debtor, asset, amt(asset, 1000), "0xAA")
	l.Freeze(debtor, asset, amt(asset, 1000))
	l.SettleWithdraw(debtor, asset, amt(asset, 1000), amt(asset, 0), "0xBB")
	if _, err := l.ReverseOnChain(debtor, asset, amt(asset, 1000), "0xAA"); err != nil {
		t.Fatal(err)
	}
	if l.BadDebtTotal(asset).Cmp(settlement.AssetAmountFromInt64(1000, 6)) != 0 {
		t.Fatalf("expect bad debt 1000, got %s", l.BadDebtTotal(asset).HumanString())
	}

	// 种子其他用户可用余额（盈利用户，作分摊基数）
	if err := l.Deposit(alice, asset, amt(asset, 4000), "seed"); err != nil {
		t.Fatal(err)
	}
	if err := l.Deposit(bob, asset, amt(asset, 6000), "seed"); err != nil {
		t.Fatal(err)
	}

	// 场景一：保险基金不足以覆盖（为 0），全部分摊 -> alice 摊 400，bob 摊 600
	detail, recovered, err := l.SocializeBadDebt(asset)
	if err != nil {
		t.Fatal(err)
	}
	if !eqAmt(recovered, 1000) {
		t.Fatalf("expect recovered 1000, got %s", recovered.HumanString())
	}
	if l.BadDebtTotal(asset).Sign() != 0 {
		t.Fatalf("bad debt should be cleared, remain %s", l.BadDebtTotal(asset).HumanString())
	}
	if l.IsOutflowRestricted(debtor, asset) {
		t.Fatal("bad debt cleared, debtor restriction should lift")
	}
	if got := detail[alice]; !eqAmt(got, 400) {
		t.Fatalf("alice share expect 400, got %s", got.HumanString())
	}
	if got := detail[bob]; !eqAmt(got, 600) {
		t.Fatalf("bob share expect 600, got %s", got.HumanString())
	}
	aliceBal, _, _ := l.Balance(alice, asset)
	bobBal, _, _ := l.Balance(bob, asset)
	if !eqAmt(aliceBal, 3600) || !eqAmt(bobBal, 5400) {
		t.Fatalf("unexpected balances alice=%s bob=%s", aliceBal.HumanString(), bobBal.HumanString())
	}

	// 场景二：重新制造坏账，验证保险基金优先冲减
	l2 := ledger.New()
	l2.ReceiveOnChain(debtor, asset, amt(asset, 500), "0xAA")
	l2.Freeze(debtor, asset, amt(asset, 500))
	l2.SettleWithdraw(debtor, asset, amt(asset, 500), amt(asset, 0), "0xBB")
	if _, err := l2.ReverseOnChain(debtor, asset, amt(asset, 500), "0xAA"); err != nil {
		t.Fatal(err)
	}
	// 保险基金预先注资 300
	if err := l2.CreditAvailable(ledger.SysInsurance, asset, amt(asset, 300), "seed_ins", "seed"); err != nil {
		t.Fatal(err)
	}
	// 再给一个盈利用户 1000 作分摊基数
	if err := l2.Deposit(alice, asset, amt(asset, 1000), "seed"); err != nil {
		t.Fatal(err)
	}
	// 坏账 500 = 保险基金冲 300 + 社会化分摊 200（alice 全摊）
	d2, rec2, err := l2.SocializeBadDebt(asset)
	if err != nil {
		t.Fatal(err)
	}
	if !eqAmt(rec2, 500) {
		t.Fatalf("expect recovered 500, got %s", rec2.HumanString())
	}
	if l2.BadDebtTotal(asset).Sign() != 0 {
		t.Fatalf("bad debt should clear, remain %s", l2.BadDebtTotal(asset).HumanString())
	}
	if got := d2[alice]; !eqAmt(got, 200) {
		t.Fatalf("alice socialized share expect 200, got %s", got.HumanString())
	}
}

// TestReconcileProductionPaths 验证：全程使用生产资金路径（ReceiveOnChain/Freeze/FreezeWithdraw/
// SettleWithdraw/ReverseOnChain/ReverseWithdraw/SocializeBadDebt）时，复式记账恒等式恒成立——
// 每个资产下所有账户（含系统负债账户）权益总和为 0。这是交易所资金安全的零知识对账不变量。
func TestReconcileProductionPaths(t *testing.T) {
	l := ledger.New()
	asset := "USDT"

	// 用户1：链上充值 10000
	if err := l.ReceiveOnChain(1, asset, amt(asset, 10000), "0x1"); err != nil {
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after deposit should balance: %v", l.Reconcile())
	}

	// 用户1：提现受理 + 结算（资金离场）
	if err := l.FreezeWithdraw(1, asset, amt(asset, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleWithdraw(1, asset, amt(asset, 1000), amt(asset, 0), "0xw1"); err != nil {
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after settle withdraw should balance: %v", l.Reconcile())
	}

	// 用户2：充值后开仓冻结保证金（不影响平衡）
	if err := l.ReceiveOnChain(2, asset, amt(asset, 5000), "0x2"); err != nil {
		t.Fatal(err)
	}
	if err := l.Freeze(2, asset, amt(asset, 500)); err != nil {
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after position freeze should balance: %v", l.Reconcile())
	}

	// 用户3：充值 -> 提现动用 -> 孤块回滚充值（造坏账，SysBadDebt 垫付）
	if err := l.ReceiveOnChain(3, asset, amt(asset, 5000), "0x3"); err != nil {
		t.Fatal(err)
	}
	if err := l.FreezeWithdraw(3, asset, amt(asset, 3000)); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleWithdraw(3, asset, amt(asset, 3000), amt(asset, 0), "0xw3"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReverseOnChain(3, asset, amt(asset, 5000), "0x3"); err != nil { // 回滚整笔充值 -> 坏账 3000
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after reverse onchain (bad debt) should balance: %v", l.Reconcile())
	}

	// 社会化分摊回收坏账（保险基金不足 -> 盈利用户分摊）
	if _, _, err := l.SocializeBadDebt(asset); err != nil {
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after socialize should balance: %v", l.Reconcile())
	}

	// 用户1：再提现并孤块回滚（提现回滚把资金退回提现冻结，不影响持仓/平衡）
	if err := l.FreezeWithdraw(1, asset, amt(asset, 2000)); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleWithdraw(1, asset, amt(asset, 2000), amt(asset, 0), "0xw4"); err != nil {
		t.Fatal(err)
	}
	if err := l.ReverseWithdraw(1, asset, amt(asset, 2000), amt(asset, 0), "0xw4"); err != nil {
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after reverse withdraw should balance: %v", l.Reconcile())
	}
}

// TestReconcileDetectsUnpairedMint 反向验证：对账能暴露未配对的凭空铸币。
// 演示用 Deposit 不配对系统负债账户，会引入非零偏差，Reconcile 必须报出。
func TestReconcileDetectsUnpairedMint(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	if err := l.Deposit(1, asset, amt(asset, 1000), "seed"); err != nil { // 凭空铸币，未配对
		t.Fatal(err)
	}
	if l.IsBalanced() {
		t.Fatal("unpaired Deposit mint should break balance, but Reconcile reports balanced")
	}
	dev := l.Reconcile()
	if !eqAmt(dev[asset], 1000) {
		t.Fatalf("expect deviation 1000 for unpaired mint, got %s", dev[asset].HumanString())
	}
}

// TestRunReconcileOnceUpdatesStats 验证 RunReconcileOnce 更新巡检快照并反映平衡态。
func TestRunReconcileOnceUpdatesStats(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	// 生产路径：充值经链上确认入账，借贷恒等 -> 平衡
	l.ReceiveOnChain(1, asset, amt(asset, 1000), "0xin")
	st := l.RunReconcileOnce()
	if !st.LastBalanced {
		t.Fatalf("production path should be balanced, got deviation %v", st.LastDeviation)
	}
	if st.LastRun.IsZero() {
		t.Fatal("LastRun should be set")
	}

	// 注入凭空铸币 -> 不平衡，偏差应被记录
	l.Deposit(2, asset, amt(asset, 500), "seed")
	st = l.RunReconcileOnce()
	if st.LastBalanced {
		t.Fatal("unpaired mint should break balance")
	}
	if !eqAmt(st.LastDeviation[asset], 500) {
		t.Fatalf("expect deviation 500, got %s", st.LastDeviation[asset].HumanString())
	}
}

// TestReconcileAlertHookFiresOnImbalance 验证不平账时告警回调被触发（监控接入点）。
func TestReconcileAlertHookFiresOnImbalance(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	got := make(chan map[string]settlement.AssetAmount, 1)
	l.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		got <- dev
	})
	l.Deposit(1, asset, amt(asset, 777), "seed") // 凭空铸币触发不平衡
	l.RunReconcileOnce()

	select {
	case dev := <-got:
		if !eqAmt(dev[asset], 777) {
			t.Fatalf("alert hook deviation expect 777, got %s", dev[asset].HumanString())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alert hook was not invoked on imbalance")
	}

	// 关闭 hook 后不再触发
	l.SetReconcileAlertHook(nil)
	l.Deposit(2, asset, amt(asset, 100), "seed")
	select {
	case <-got:
		t.Fatal("alert hook should not fire after being cleared")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestReconcilerGoroutineStartStop 验证后台巡检 goroutine 定时运行且可干净停止（幂等）。
func TestReconcilerGoroutineStartStop(t *testing.T) {
	l := ledger.New()
	l.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {})
	l.Deposit(1, "USDT", amt("USDT", 100), "seed") // 持续不平衡，巡检会反复累加 ImbalanceCount

	l.StartReconciler(50 * time.Millisecond)
	l.StartReconciler(50 * time.Millisecond) // 幂等：不应启动第二个

	// 等待至少一次定时巡检
	deadline := time.Now().Add(2 * time.Second)
	for {
		if !l.LastReconcile().LastRun.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background reconciler never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	countAtStop := l.LastReconcile().ImbalanceCount
	if countAtStop == 0 {
		t.Fatal("expect at least one imbalance counted before stop")
	}

	l.StopReconciler()
	l.StopReconciler() // 幂等停止不报错

	// 停止后 ImbalanceCount 不应继续增长（goroutine 已退出）
	time.Sleep(200 * time.Millisecond)
	if l.LastReconcile().ImbalanceCount != countAtStop {
		t.Fatalf("reconciler still running after Stop: count %d -> %d",
			countAtStop, l.LastReconcile().ImbalanceCount)
	}
}

// TestUserLevelRestrictionLift 验证"用户级精确解限"：多债务人场景下，某用户补缴自身
// 坏账后仅解除其自身出金限制，不连坐其他未结清债务人；全局结清（他人兜底/社会化）才全部解限。
func TestUserLevelRestrictionLift(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	const A, B int64 = 7101, 7102

	// 注意：先完成两笔充值，再做两笔回滚，避免回滚之间插入充值触发坏账自动回收的交叉影响。
	// 制造 A 坏账 1000：充值 5000 -> 提现动用 1000 -> 回滚（垫付 1000）
	l.ReceiveOnChain(A, asset, amt(asset, 5000), "0xAin")
	// 制造 B 坏账 2000：充值 8000 -> 提现动用 2000 -> 回滚（垫付 2000）
	l.ReceiveOnChain(B, asset, amt(asset, 8000), "0xBin")
	l.FreezeWithdraw(A, asset, amt(asset, 1000))
	l.SettleWithdraw(A, asset, amt(asset, 1000), amt(asset, 0), "0xAout")
	l.FreezeWithdraw(B, asset, amt(asset, 2000))
	l.SettleWithdraw(B, asset, amt(asset, 2000), amt(asset, 0), "0xBout")
	l.ReverseOnChain(A, asset, amt(asset, 5000), "0xAin")
	l.ReverseOnChain(B, asset, amt(asset, 8000), "0xBin")

	if !l.IsOutflowRestricted(A, asset) || !l.IsOutflowRestricted(B, asset) {
		t.Fatal("both debtors should be restricted after bad debt")
	}
	if got := l.BadDebtTotal(asset); !eqAmt(got, 3000) {
		t.Fatalf("expect total bad debt 3000, got %s", got.HumanString())
	}

	// 给 A/B 可用余额（演示用 CreditAvailable 不触发自动回收，仅用于模拟后续补缴资金）
	l.CreditAvailable(A, asset, amt(asset, 5000), "topup", "topup")
	l.CreditAvailable(B, asset, amt(asset, 5000), "topup", "topup")

	// A 仅补缴自身 1000 -> 仅 A 解限，B 仍受限
	if err := l.RepayBadDebt(A, asset, amt(asset, 1000), "repayA"); err != nil {
		t.Fatal(err)
	}
	if l.IsOutflowRestricted(A, asset) {
		t.Fatal("A repaid own debt, should be lifted")
	}
	if !l.IsOutflowRestricted(B, asset) {
		t.Fatal("B still owes, must remain restricted (no false lift)")
	}
	if got := l.BadDebtTotal(asset); !eqAmt(got, 2000) {
		t.Fatalf("expect remaining bad debt 2000, got %s", got.HumanString())
	}

	// B 补缴 2000 -> 全局结清，全部解限
	if err := l.RepayBadDebt(B, asset, amt(asset, 2000), "repayB"); err != nil {
		t.Fatal(err)
	}
	if l.IsOutflowRestricted(A, asset) || l.IsOutflowRestricted(B, asset) {
		t.Fatal("all debt cleared, both should be lifted")
	}
	if got := l.BadDebtTotal(asset); got.Sign() != 0 {
		t.Fatalf("expect zero bad debt, got %s", got.HumanString())
	}
}

// TestVoluntaryRepayCoversOthers 验证用户自愿多缴可替其他债务人兜底：A 多缴覆盖 B 的欠款，
// 全局结清后两人皆解限。
func TestVoluntaryRepayCoversOthers(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	const A, B int64 = 7201, 7202
	l.ReceiveOnChain(A, asset, amt(asset, 5000), "0xAin")
	l.ReceiveOnChain(B, asset, amt(asset, 8000), "0xBin")
	l.FreezeWithdraw(A, asset, amt(asset, 1000))
	l.SettleWithdraw(A, asset, amt(asset, 1000), amt(asset, 0), "0xAout")
	l.FreezeWithdraw(B, asset, amt(asset, 2000))
	l.SettleWithdraw(B, asset, amt(asset, 2000), amt(asset, 0), "0xBout")
	l.ReverseOnChain(A, asset, amt(asset, 5000), "0xAin")
	l.ReverseOnChain(B, asset, amt(asset, 8000), "0xBin")
	l.CreditAvailable(A, asset, amt(asset, 5000), "topup", "topup")

	// A 缴 3000（自身 1000 + 替 B 兜底 2000）-> 全局结清，全部解限
	if err := l.RepayBadDebt(A, asset, amt(asset, 3000), "repayA"); err != nil {
		t.Fatal(err)
	}
	if l.IsOutflowRestricted(A, asset) || l.IsOutflowRestricted(B, asset) {
		t.Fatal("A covered both, both should be lifted")
	}
	if got := l.BadDebtTotal(asset); got.Sign() != 0 {
		t.Fatalf("expect zero bad debt, got %s", got.HumanString())
	}
}

// TestSocializeGovernance 验证社会化分摊治理审批流：propose 仅预览不动账本，approve 凭
// 提案号执行；坏账来源方（受限）不参与分摊，仅非受限盈利用户按比例承担。
func TestSocializeGovernance(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	const debtor, p1, p2 int64 = 7301, 7302, 7303

	// 制造坏账 1000（先充值再回滚）
	l.ReceiveOnChain(debtor, asset, amt(asset, 5000), "0xdin")
	l.FreezeWithdraw(debtor, asset, amt(asset, 1000))
	l.SettleWithdraw(debtor, asset, amt(asset, 1000), amt(asset, 0), "0xdout")
	l.ReverseOnChain(debtor, asset, amt(asset, 5000), "0xdin")
	if l.BadDebtTotal(asset).Cmp(settlement.AssetAmountFromInt64(999, 6)) < 0 {
		t.Fatalf("expect bad debt ~1000, got %s", l.BadDebtTotal(asset).HumanString())
	}

	// 非受限盈利用户
	l.CreditAvailable(p1, asset, amt(asset, 4000), "seed", "seed")
	l.CreditAvailable(p2, asset, amt(asset, 6000), "seed", "seed")

	// 无坏账时 propose 报错（反向）
	if _, _, err := l.ProposeSocialize("BTC"); err == nil {
		t.Fatal("propose on no bad debt should error")
	}

	// propose：仅预览，不动账本
	id, preview, err := l.ProposeSocialize(asset)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Status != "pending" || preview.Recovered.Cmp(settlement.AssetAmountFromInt64(999, 6)) < 0 {
		t.Fatalf("preview unexpected: %+v", preview)
	}
	if _, ok := preview.Detail[debtor]; ok {
		t.Fatal("debtor (restricted) must NOT be in socialize shares")
	}
	if got := preview.Detail[p1]; !eqAmt(got, 400) {
		t.Fatalf("p1 share expect 400, got %s", got.HumanString())
	}
	if got := preview.Detail[p2]; !eqAmt(got, 600) {
		t.Fatalf("p2 share expect 600, got %s", got.HumanString())
	}
	// propose 后账本未变（坏账仍在、限制仍在）
	if l.BadDebtTotal(asset).Cmp(settlement.AssetAmountFromInt64(999, 6)) < 0 {
		t.Fatal("propose must not mutate ledger")
	}
	if !l.IsOutflowRestricted(debtor, asset) {
		t.Fatal("debtor should still be restricted before approval")
	}

	// approve 错误提案号失败
	if _, _, err := l.ApproveSocialize(asset, "WRONG"); err == nil {
		t.Fatal("approve with wrong id should error")
	}

	// approve 正确提案号：执行分摊
	detail, recovered, err := l.ApproveSocialize(asset, id)
	if err != nil {
		t.Fatal(err)
	}
	if !eqAmt(recovered, 1000) {
		t.Fatalf("recovered expect 1000, got %s", recovered.HumanString())
	}
	if l.BadDebtTotal(asset).Sign() != 0 {
		t.Fatalf("bad debt should be cleared, got %s", l.BadDebtTotal(asset).HumanString())
	}
	if l.IsOutflowRestricted(debtor, asset) {
		t.Fatal("debtor should be lifted after socialize clears bad debt")
	}
	if got := detail[p1]; !eqAmt(got, 400) {
		t.Fatalf("p1 share expect 400, got %s", got.HumanString())
	}
}

// TestSnapshotRestore 验证账本持久化：构造余额/坏账/出金限制/治理提案后，快照到新账本，
// 恢复后资金安全状态（可用/冻结/提现冻结、坏账归属、出金限制、提案、对账平衡）完全一致。
func TestSnapshotRestore(t *testing.T) {
	l := ledger.New()
	asset := "USDT"

	// 余额：可用/持仓冻结/提现冻结三种形态都造值，验证快照覆盖全部字段。
	_ = l.Deposit(1, asset, amt(asset, 50000), "seed")
	_ = l.Freeze(1, asset, amt(asset, 8000))         // 持仓保证金冻结
	_ = l.FreezeWithdraw(1, asset, amt(asset, 2000)) // 提现冻结
	// 坏账来源方：可用余额较少，充值回滚时将产生交易所垫付坏账。
	_ = l.Deposit(2, asset, amt(asset, 10000), "seed")

	// 坏账闭环：充值回滚制造坏账（用户可用不足以全额回拨）+ 出金限制 + 坏账归属。
	bd, err := l.ReverseOnChain(2, asset, amt(asset, 30000), "txbad")
	if err != nil {
		t.Fatal(err)
	}
	if !eqAmt(bd, 20000) {
		t.Fatalf("expect bad debt 20000, got %s", bd.HumanString())
	}
	if !l.IsOutflowRestricted(2, asset) {
		t.Fatal("debtor should be restricted after bad debt")
	}
	if !eqAmt(l.BadDebtTotal(asset), 20000) {
		t.Fatal("expect outstanding bad debt 20000")
	}

	// 治理提案：先 propose 一笔待审批提案，验证提案也能持久化恢复。
	propID, _, err := l.ProposeSocialize(asset)
	if err != nil {
		t.Fatal(err)
	}

	// 提现地址白名单：预登记并验证一条地址，验证恢复后白名单完好。
	if _, err := l.AddWithdrawAddress(1, asset, "ETH", "0xwhite", "recovery"); err != nil {
		t.Fatal(err)
	}
	if err := l.ConfirmWithdrawAddress(1, asset, "ETH", "0xwhite"); err != nil {
		t.Fatal(err)
	}

	// 落盘 + 读回（演示路径 Deposit 会引入偏差，验证恢复后保持同一偏差，不变量一致）。
	if err := l.SaveToFile("/tmp/ledger_snap_test.json"); err != nil {
		t.Fatal(err)
	}
	snap, err := ledger.LoadSnapshotFromFile("/tmp/ledger_snap_test.json")
	if err != nil {
		t.Fatal(err)
	}

	// 新建账本并恢复。
	l2 := ledger.New()
	l2.Restore(snap)

	// 1) 余额三态恢复一致
	assertBalance(t, l2, 1, asset, 50000-8000-2000, 8000)
	if wf, ok := l2.WithdrawFrozenBalance(1, asset); !ok || !eqAmt(wf, 2000) {
		t.Fatalf("user1 withdraw frozen = %s, want 2000", wf.HumanString())
	}
	assertBalance(t, l2, 2, asset, 0, 0) // 坏账回滚已扣减可用至 0

	// 2) 坏账归属与出金限制恢复一致
	if !l2.IsOutflowRestricted(2, asset) {
		t.Fatal("restriction should survive restore")
	}
	if !eqAmt(l2.BadDebtTotal(asset), 20000) {
		t.Fatalf("bad debt total mismatch after restore: %s", l2.BadDebtTotal(asset).HumanString())
	}

	// 3) 治理提案恢复一致（pending 提案仍需后续 approve）
	if _, ok := lookupProposal(t, l2, asset, propID); !ok {
		t.Fatal("socialize proposal should survive restore")
	}

	// 3.5) 提现地址白名单恢复一致（已验证地址仍可用）
	found := false
	for _, a := range l2.ListWithdrawAddresses(1) {
		if a.Address == "0xwhite" && a.Verified {
			found = true
		}
	}
	if !found {
		t.Fatal("whitelisted address should survive restore")
	}

	// 4) 对账不变量跨恢复保持一致（偏差相等）
	dev1 := l.Reconcile()
	dev2 := l2.Reconcile()
	if dev1[asset].Cmp(dev2[asset]) != 0 {
		t.Fatalf("reconcile deviation mismatch after restore: %s vs %s", dev1[asset].HumanString(), dev2[asset].HumanString())
	}

	// 5) 恢复后坏账回收闭环依然可用：用户2链上充值回填应冲抵归属并解限。
	if err := l2.ReceiveOnChain(2, asset, amt(asset, 30000), "txrepay"); err != nil {
		t.Fatal(err)
	}
	if l2.IsOutflowRestricted(2, asset) {
		t.Fatal("debtor restriction should lift after repayment via restored ledger")
	}
	if l2.BadDebtTotal(asset).Sign() != 0 {
		t.Fatalf("bad debt should be cleared after repay, got %s", l2.BadDebtTotal(asset).HumanString())
	}
}

// lookupProposal 在恢复后的账本中查找某资产的治理提案（从 Snapshot 取值比对）。
func lookupProposal(t *testing.T, l *ledger.Ledger, asset, propID string) (ledger.SocializeProposal, bool) {
	t.Helper()
	snap := l.Snapshot()
	p, ok := snap.SocializeProposals[asset]
	if !ok || p.ID != propID {
		return ledger.SocializeProposal{}, false
	}
	return p, true
}

// TestHotColdWalletSweep 验证冷热钱包分离与自动归集的资金安全防线：
//  1) 充值入账同步热钱包库存，且热钱包+冷钱包 恒等于 -SysChainClearing（偿付能力不变量）；
//  2) 设上限后超额充值自动归集冷钱包，热钱包敞口被收敛到上限；
//  3) 提现使热钱包库存减少（资金离链到用户外部地址），不变量仍成立；
//  4) 手工归集/回调正确搬迁库存，且不改变对用户负债与链上总额；
//  5) 孤块回滚对库存对称回拨，不变量仍成立。
func TestHotColdWalletSweep(t *testing.T) {
	l := ledger.New()
	asset := "USDT"

	// 不设上限：充值 100k 全部留在热钱包。
	_ = l.ReceiveOnChain(1, asset, amt(asset, 100000), "tx1")
	if !eqAmt(l.HotWalletBalance(asset), 100000) {
		t.Fatalf("hot wallet = %s, want 100000", l.HotWalletBalance(asset).HumanString())
	}
	if !eqAmt(l.InventoryMatchesLiability(asset), 0) {
		t.Fatalf("inventory mismatch = %s, want 0", l.InventoryMatchesLiability(asset).HumanString())
	}

	// 设热钱包上限 50000：此前 100k 已超限，设置即收敛——自动归集 50000 到冷钱包。
	l.SetHotWalletCap(asset, amt(asset, 50000))
	if !eqAmt(l.HotWalletBalance(asset), 50000) {
		t.Fatalf("hot wallet after cap reconcile = %s, want 50000", l.HotWalletBalance(asset).HumanString())
	}
	if !eqAmt(l.ColdWalletBalance(asset), 50000) {
		t.Fatalf("cold wallet after auto-sweep = %s, want 50000", l.ColdWalletBalance(asset).HumanString())
	}
	if !eqAmt(l.HotWalletExcess(asset), 0) {
		t.Fatalf("hot excess should be 0 after auto-sweep, got %s", l.HotWalletExcess(asset).HumanString())
	}

	// 用户1提现 30000（含 500 手续费）：热钱包减少 total，冷钱包不变，不变量仍成立。
	if err := l.FreezeWithdraw(1, asset, amt(asset, 30000)); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleWithdraw(1, asset, amt(asset, 29500), amt(asset, 500), "txw1"); err != nil {
		t.Fatal(err)
	}
	if !eqAmt(l.HotWalletBalance(asset), 20000) {
		t.Fatalf("hot wallet after withdraw = %s, want 20000", l.HotWalletBalance(asset).HumanString())
	}
	if !eqAmt(l.InventoryMatchesLiability(asset), 0) {
		t.Fatalf("inventory mismatch after withdraw = %s", l.InventoryMatchesLiability(asset).HumanString())
	}

	// 手工归集 15000 到冷钱包，再回调 5000 回热钱包：链上总额与负债不变（内部转账）。
	if swept, err := l.SweepToCold(asset, amt(asset, 15000)); err != nil || !eqAmt(swept, 15000) {
		t.Fatalf("sweep failed: swept=%s err=%v", swept.HumanString(), err)
	}
	if !eqAmt(l.HotWalletBalance(asset), 5000) || !eqAmt(l.ColdWalletBalance(asset), 65000) {
		t.Fatalf("after sweep hot=%s cold=%s, want 5000/65000", l.HotWalletBalance(asset).HumanString(), l.ColdWalletBalance(asset).HumanString())
	}
	if moved, err := l.UnsweepFromCold(asset, amt(asset, 5000)); err != nil || !eqAmt(moved, 5000) {
		t.Fatalf("unsweep failed: moved=%s err=%v", moved.HumanString(), err)
	}
	if !eqAmt(l.HotWalletBalance(asset), 10000) || !eqAmt(l.ColdWalletBalance(asset), 60000) {
		t.Fatalf("after unsweep hot=%s cold=%s, want 10000/60000", l.HotWalletBalance(asset).HumanString(), l.ColdWalletBalance(asset).HumanString())
	}
	if !eqAmt(l.HotWalletBalance(asset).Add(l.ColdWalletBalance(asset)), 70000) {
		t.Fatalf("total on-chain should stay 70000, got %s", l.HotWalletBalance(asset).Add(l.ColdWalletBalance(asset)).HumanString())
	}

	// 孤块回滚：充值被丢弃 → 热钱包对称回拨 100000（从未真正收到），不变量仍成立。
	if _, err := l.ReverseOnChain(1, asset, amt(asset, 100000), "tx1"); err != nil {
		t.Fatal(err)
	}
	// hot: 10000 - 100000 = -90000；cold 60000；总额 -30000 == -SysChainClearing。
	if !eqAmt(l.HotWalletBalance(asset), -90000) {
		t.Fatalf("hot wallet after rollback = %s, want -90000 (never received)", l.HotWalletBalance(asset).HumanString())
	}
	if !eqAmt(l.InventoryMatchesLiability(asset), 0) {
		t.Fatalf("inventory mismatch after rollback = %s", l.InventoryMatchesLiability(asset).HumanString())
	}
}

// TestWithdrawHoldPeriod 验证提现安全冷静期 + 全局紧急冻结 + 每日限额三重风控：
//  1) 冷静期内拒绝链上清算（资金仅冻结，未离链）；
//  2) 全局紧急冻结拦截一切出金受理；
//  3) 撤销提现退回冻结资金并退还当日预占额度；
//  4) 每日限额约束单用户单日累计提现（含预占）；
//  5) 冷静期过后清算成功，资金真正离开系统（WithdrawFrozen 归零）。
func TestWithdrawHoldPeriod(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	l.SetWithdrawHoldPeriod(100 * time.Millisecond)
	l.SetDailyWithdrawLimit(asset, amt(asset, 15000))

	uid := int64(9001)
	_ = l.Deposit(uid, asset, amt(asset, 100000), "seed")
	assertBalance(t, l, uid, asset, 100000, 0)

	// 提现地址白名单：预登记并验证 "0xout"（演示期 addressVerifyPeriod=0，验证后即可用）。
	if _, err := l.AddWithdrawAddress(uid, asset, "ETH", "0xout", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := l.ConfirmWithdrawAddress(uid, asset, "ETH", "0xout"); err != nil {
		t.Fatal(err)
	}

	// wf 校验提现冻结余额（独立于持仓保证金冻结 a.Frozen）。
	wf := func(want float64) {
		t.Helper()
		v, ok := l.WithdrawFrozenBalance(uid, asset)
		if !ok {
			v = settlement.AssetAmount{}
		}
		if !eqAmt(v, want) {
			t.Fatalf("user %d withdraw frozen = %s, want %.8f", uid, v.HumanString(), want)
		}
	}

	// 1) 受理进入冷静期：资金冻结但未离链（Available 扣减，WithdrawFrozen 增加）。
	id, holdUntil, err := l.RequestWithdrawHold(uid, asset, amt(asset, 10000), amt(asset, 0), "ETH", "0xout")
	if err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 90000, 0)
	wf(10000)
	if !time.Now().Before(holdUntil) {
		t.Fatal("holdUntil should be in the future")
	}

	// 2) 冷静期内拒绝清算（资金仍冻结在用户侧，未离链）。
	if _, e := l.FinalizeWithdrawHold(id); e == nil {
		t.Fatal("finalize during cooling period should fail")
	}
	assertBalance(t, l, uid, asset, 90000, 0)
	wf(10000)

	// 3) 撤销退回冻结资金与当日预占额度，再验证全局冻结拦截。
	if err := l.CancelWithdrawHold(id); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 100000, 0)
	wf(0)
	l.SetGlobalWithdrawalFreeze(true)
	if _, _, e := l.RequestWithdrawHold(uid, asset, amt(asset, 5000), amt(asset, 0), "ETH", "0xout"); e == nil {
		t.Fatal("request during global freeze should fail")
	}
	l.SetGlobalWithdrawalFreeze(false)

	// 4) 每日限额：累计 10000 OK，再 10000 超 15000 被拒；撤销后可再次受理。
	id1, _, err := l.RequestWithdrawHold(uid, asset, amt(asset, 10000), amt(asset, 0), "ETH", "0xout")
	if err != nil {
		t.Fatal(err)
	}
	wf(10000)
	if _, _, e := l.RequestWithdrawHold(uid, asset, amt(asset, 10000), amt(asset, 0), "ETH", "0xout"); e == nil {
		t.Fatal("second request exceeding daily limit should fail")
	}
	if err := l.CancelWithdrawHold(id1); err != nil {
		t.Fatal(err)
	}
	wf(0)
	id2, _, err := l.RequestWithdrawHold(uid, asset, amt(asset, 10000), amt(asset, 0), "ETH", "0xout")
	if err != nil {
		t.Fatal(err)
	}
	wf(10000)

	// 5) 等待冷静期过后清算成功（资金真正离开系统，WithdrawFrozen 归零）。
	time.Sleep(150 * time.Millisecond)
	e, err := l.FinalizeWithdrawHold(id2)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Finalized {
		t.Fatal("hold should be finalized")
	}
	assertBalance(t, l, uid, asset, 90000, 0) // 可用保持 90000（request 时已从 100000 扣）
	wf(0)
	if l.PendingWithdrawHoldCount() != 0 {
		t.Fatalf("pending holds should be 0 after finalize, got %d", l.PendingWithdrawHoldCount())
	}
}

// TestWithdrawAddressWhitelist 验证提现地址白名单 + 新地址验证冷静期风控：
//  1) 预登记地址默认未验证，即便已验证仍需度过验证冷静期方可首次提现；
//  2) 验证冷静期内以该地址提现被拒（"not whitelisted/verified"）；
//  3) 验证冷静期过后该地址可正常进入提现冷静期并清算；
//  4) 未登记地址提现被拒；重复登记/验证不存在地址返回错误；
//  5) 撤销地址后该地址提现被拒；恢复后白名单状态完好。
func TestWithdrawAddressWhitelist(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	l.SetWithdrawHoldPeriod(50 * time.Millisecond)
	l.SetAddressVerifyPeriod(100 * time.Millisecond) // 新地址验证冷静期 100ms
	l.SetDailyWithdrawLimit(asset, amt(asset, 1e9))  // 放大日限额，聚焦白名单逻辑

	uid := int64(9101)
	_ = l.Deposit(uid, asset, amt(asset, 100000), "seed")

	// 1) 预登记地址（默认未验证）。
	addr, err := l.AddWithdrawAddress(uid, asset, "ETH", "0xsafe", "main")
	if err != nil {
		t.Fatal(err)
	}
	if addr.Verified {
		t.Fatal("newly registered address should be unverified")
	}
	// 未验证：提现被拒。
	if _, _, e := l.RequestWithdrawHold(uid, asset, amt(asset, 1000), amt(asset, 0), "ETH", "0xsafe"); e == nil {
		t.Fatal("unverified address should be rejected")
	}

	// 2) 验证后仍在验证冷静期内：仍被拒。
	if err := l.ConfirmWithdrawAddress(uid, asset, "ETH", "0xsafe"); err != nil {
		t.Fatal(err)
	}
	if _, _, e := l.RequestWithdrawHold(uid, asset, amt(asset, 1000), amt(asset, 0), "ETH", "0xsafe"); e == nil {
		t.Fatal("address within verify cooldown should be rejected")
	}

	// 3) 等待验证冷静期过后：地址可用，进入提现冷静期并清算成功。
	time.Sleep(150 * time.Millisecond)
	id, _, err := l.RequestWithdrawHold(uid, asset, amt(asset, 1000), amt(asset, 0), "ETH", "0xsafe")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // 等提现冷静期
	if _, err := l.FinalizeWithdrawHold(id); err != nil {
		t.Fatal(err)
	}

	// 4) 未登记地址提现被拒；重复登记报错；验证不存在地址报错。
	if _, _, e := l.RequestWithdrawHold(uid, asset, amt(asset, 1000), amt(asset, 0), "ETH", "0xevil"); e == nil {
		t.Fatal("unregistered address should be rejected")
	}
	if _, err := l.AddWithdrawAddress(uid, asset, "ETH", "0xsafe", "dup"); err == nil {
		t.Fatal("duplicate registration should error")
	}
	if err := l.ConfirmWithdrawAddress(uid, asset, "ETH", "0xghost"); err == nil {
		t.Fatal("confirm non-existent address should error")
	}

	// 5) 撤销地址后该地址提现被拒；恢复操作不影响余额（验证撤销仅移出白名单）。
	if err := l.RemoveWithdrawAddress(uid, asset, "ETH", "0xsafe"); err != nil {
		t.Fatal(err)
	}
	if _, _, e := l.RequestWithdrawHold(uid, asset, amt(asset, 1000), amt(asset, 0), "ETH", "0xsafe"); e == nil {
		t.Fatal("removed address should be rejected")
	}
	// 撤销不存在地址报错。
	if err := l.RemoveWithdrawAddress(uid, asset, "ETH", "0xghost"); err == nil {
		t.Fatal("remove non-existent address should error")
	}
}

// TestRiskEngineAutoFreeze 验证可疑行为风控引擎：提现速率骤增（次数/累计额）或短时间大量
// 新增地址会触发自动全局冻结并留痕；人工 resume 后清零"自动冻结"标记；事件可持久化恢复。
func TestRiskEngineAutoFreeze(t *testing.T) {
	asset := "USDT"

	// --- 场景1：提现请求次数触发自动冻结 ---
	l := ledger.New()
	l.SetWithdrawHoldPeriod(50 * time.Millisecond)
	l.SetAddressVerifyPeriod(0) // 地址立即可用，聚焦风控逻辑
	l.EnableRiskEngine(true, true)
	l.SetRiskThresholds(60*time.Second, 0, 3, 0) // 次数阈值=3，累计额/地址突增不触发

	uid := int64(9201)
	_ = l.Deposit(uid, asset, amt(asset, 100000), "seed")
	_, _ = l.AddWithdrawAddress(uid, asset, "ETH", "0xsafe", "main")
	_ = l.ConfirmWithdrawAddress(uid, asset, "ETH", "0xsafe")

	// 前两次提现正常受理（次数=1,2 < 3）。
	if _, _, err := l.RequestWithdrawHold(uid, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe"); err != nil {
		t.Fatalf("1st withdraw should pass: %v", err)
	}
	if _, _, err := l.RequestWithdrawHold(uid, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe"); err != nil {
		t.Fatalf("2nd withdraw should pass: %v", err)
	}
	// 第三次：次数达阈值，触发自动全局冻结并拒绝本次受理。
	if _, _, err := l.RequestWithdrawHold(uid, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe"); err == nil {
		t.Fatal("3rd withdraw should be blocked by risk auto-freeze")
	}
	if !l.IsGlobalWithdrawalFrozen() {
		t.Fatal("global freeze should be engaged after risk trigger")
	}
	if !l.AutoFrozenByRisk() {
		t.Fatal("auto_frozen_by_risk flag should be set")
	}
	evs := l.ListRiskEvents(0)
	if len(evs) != 1 || evs[0].Type != "withdraw_velocity" || evs[0].Action != "auto_global_freeze" {
		t.Fatalf("expected 1 withdraw_velocity auto-freeze event, got %+v", evs)
	}
	// 冻结后任何提现均被拒。
	if _, _, err := l.RequestWithdrawHold(uid, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe"); err == nil {
		t.Fatal("withdraw should stay blocked while frozen")
	}

	// 人工 resume 后清零自动冻结标记。
	l.SetGlobalWithdrawalFreeze(false)
	l.ClearRiskAutoFreeze()
	if l.AutoFrozenByRisk() {
		t.Fatal("auto_frozen_by_risk flag should clear after manual resume")
	}
	if l.IsGlobalWithdrawalFrozen() {
		t.Fatal("global freeze should be off after resume")
	}

	// --- 场景2：提现累计额触发自动冻结 ---
	l2 := ledger.New()
	l2.SetWithdrawHoldPeriod(50 * time.Millisecond)
	l2.SetAddressVerifyPeriod(0)
	l2.EnableRiskEngine(true, true)
	l2.SetRiskThresholds(60*time.Second, 250, 0, 0) // 累计额阈值=250，次数不触发
	uid2 := int64(9202)
	_ = l2.Deposit(uid2, asset, amt(asset, 100000), "seed")
	_, _ = l2.AddWithdrawAddress(uid2, asset, "ETH", "0xsafe", "main")
	_ = l2.ConfirmWithdrawAddress(uid2, asset, "ETH", "0xsafe")
	if _, _, err := l2.RequestWithdrawHold(uid2, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe"); err != nil {
		t.Fatalf("1st (100) should pass: %v", err)
	}
	if _, _, err := l2.RequestWithdrawHold(uid2, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe"); err != nil {
		t.Fatalf("2nd (sum 200) should pass: %v", err)
	}
	if _, _, err := l2.RequestWithdrawHold(uid2, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe"); err == nil {
		t.Fatal("3rd (sum 300>=250) should trigger freeze")
	}
	if !l2.AutoFrozenByRisk() {
		t.Fatal("amount-threshold should auto-freeze")
	}

	// --- 场景3：短时间大量新增地址触发自动冻结 ---
	l3 := ledger.New()
	l3.EnableRiskEngine(true, true)
	l3.SetRiskThresholds(60*time.Second, 0, 0, 2) // 地址突增阈值=2
	uid3 := int64(9203)
	if _, err := l3.AddWithdrawAddress(uid3, asset, "ETH", "0xa1", "x"); err != nil {
		t.Fatal(err)
	}
	if l3.IsGlobalWithdrawalFrozen() {
		t.Fatal("1 address should not trigger freeze")
	}
	if _, err := l3.AddWithdrawAddress(uid3, asset, "ETH", "0xa2", "x"); err != nil {
		t.Fatal(err)
	}
	if !l3.IsGlobalWithdrawalFrozen() || !l3.AutoFrozenByRisk() {
		t.Fatal("address burst (2) should auto-freeze")
	}

	// --- 场景4：风控事件随快照持久化恢复 ---
	l4 := ledger.New()
	l4.EnableRiskEngine(true, true)
	l4.SetRiskThresholds(60*time.Second, 0, 2, 0)
	uid4 := int64(9204)
	_ = l4.Deposit(uid4, asset, amt(asset, 100000), "seed")
	_, _ = l4.AddWithdrawAddress(uid4, asset, "ETH", "0xsafe", "main")
	_ = l4.ConfirmWithdrawAddress(uid4, asset, "ETH", "0xsafe")
	_, _, _ = l4.RequestWithdrawHold(uid4, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe")
	_, _, _ = l4.RequestWithdrawHold(uid4, asset, amt(asset, 100), amt(asset, 0), "ETH", "0xsafe") // 2次触发
	if !l4.AutoFrozenByRisk() {
		t.Fatal("l4 should auto-freeze")
	}
	snap := l4.Snapshot()
	l5 := ledger.New()
	l5.Restore(snap)
	if l5.RiskEventCount() != 1 {
		t.Fatalf("risk events should survive restore, got %d", l5.RiskEventCount())
	}
	if !l5.AutoFrozenByRisk() {
		t.Fatal("auto_frozen_by_risk flag should survive restore")
	}
}

// TestWithdrawBroadcastIdempotent 验证 ClaimWithdrawBroadcast 的原子占位语义（F1 防护机制）：
// 同一 hold 重复 Claim 应仅首次返回 already=false，其余复用既有 txHash，杜绝重复链上广播。
func TestWithdrawBroadcastIdempotent(t *testing.T) {
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	l.SetAddressVerifyPeriod(0)
	_, _ = l.AddWithdrawAddress(1, "USDT", "Ethereum", "0xabc", "test")
	if err := l.ConfirmWithdrawAddress(1, "USDT", "Ethereum", "0xabc"); err != nil {
		t.Fatal(err)
	}
	id, _, err := l.RequestWithdrawHold(1, "USDT", amt("USDT", 100), amt("USDT", 1), "Ethereum", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	// 首次 Claim 取得广播权（already=false，txHash 暂空）。
	tx, already, err := l.ClaimWithdrawBroadcast(id)
	if err != nil || already || tx != "" {
		t.Fatalf("first claim: tx=%q already=%v err=%v", tx, already, err)
	}
	// 回填链上 txHash 后再次 Claim 应复用。
	if err := l.SetWithdrawTxHash(id, "0xTX"); err != nil {
		t.Fatal(err)
	}
	tx, already, err = l.ClaimWithdrawBroadcast(id)
	if err != nil || !already || tx != "0xTX" {
		t.Fatalf("second claim should reuse: tx=%q already=%v err=%v", tx, already, err)
	}
	// 广播失败回退：Reset 后应可再次取得广播权。
	if err := l.ResetWithdrawBroadcast(id); err != nil {
		t.Fatal(err)
	}
	tx, already, err = l.ClaimWithdrawBroadcast(id)
	if err != nil || already {
		t.Fatalf("after reset should reclaim: tx=%q already=%v err=%v", tx, already, err)
	}
}

// TestWithdrawBroadcastConcurrent 并发 Claim 同一 hold，恰好一个取得广播权（F1 防并发双提现）。
func TestWithdrawBroadcastConcurrent(t *testing.T) {
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	l.SetAddressVerifyPeriod(0)
	_, _ = l.AddWithdrawAddress(1, "USDT", "Ethereum", "0xabc", "test")
	if err := l.ConfirmWithdrawAddress(1, "USDT", "Ethereum", "0xabc"); err != nil {
		t.Fatal(err)
	}
	id, _, err := l.RequestWithdrawHold(1, "USDT", amt("USDT", 100), amt("USDT", 1), "Ethereum", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	var first int32
	var mu sync.Mutex
	var errs []error
	reuses := 0
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			tx, already, e := l.ClaimWithdrawBroadcast(id)
			mu.Lock()
			if e != nil {
				errs = append(errs, e)
			} else if already {
				reuses++
			} else {
				atomic.AddInt32(&first, 1)
				_ = tx
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("unexpected claim errors: %v", errs)
	}
	if atomic.LoadInt32(&first) != 1 {
		t.Fatalf("exactly one first-claim expected, got %d", atomic.LoadInt32(&first))
	}
	if reuses != n-1 {
		t.Fatalf("expected %d reuses, got %d", n-1, reuses)
	}
}

// countBizRef 统计账本流水中指定 biz+ref 的条数（每笔 Transfer 写入借/贷 2 条）。
func countBizRef(l *ledger.Ledger, biz, ref string) int {
	n := 0
	for _, e := range l.Log() {
		if e.BizType == biz && e.Ref == ref {
			n++
		}
	}
	return n
}

// TestTransferIdempotent 同参 Transfer 重复提交为幂等 no-op：余额不变、流水不翻倍。
func TestTransferIdempotent(t *testing.T) {
	l := ledger.New()
	_ = l.Deposit(1, "USDT", amt("USDT", 10000), "seed1")
	_ = l.Deposit(2, "USDT", amt("USDT", 0), "seed2")

	if err := l.Transfer(1, 2, "USDT", amt("USDT", 100), "trade", "r1"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 9900, 0)
	assertBalance(t, l, 2, "USDT", 100, 0)
	if got := countBizRef(l, "trade", "r1"); got != 2 {
		t.Fatalf("after 1st transfer expect 2 entries, got %d", got)
	}

	// 重试同一笔转账：必须幂等跳过，不双付。
	if err := l.Transfer(1, 2, "USDT", amt("USDT", 100), "trade", "r1"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 9900, 0) // 余额不变
	assertBalance(t, l, 2, "USDT", 100, 0)
	if got := countBizRef(l, "trade", "r1"); got != 2 {
		t.Fatalf("after retry expect still 2 entries (no double pay), got %d", got)
	}
}

// TestTransferTwoLegsSameRef 同一交易两腿复用同一 ref 时，两条腿都应生效（不被误杀）。
// 这证明"按完整元组去重"而非"按 (biz,ref) 去重"——后者会跳过第二条腿。
func TestTransferTwoLegsSameRef(t *testing.T) {
	l := ledger.New()
	_ = l.Deposit(1, "USDT", amt("USDT", 10000), "seed1")
	_ = l.Deposit(2, "BTC", amt("BTC", 10), "seed2")

	// 计价腿：买方付 USDT；基础腿：卖方付 BTC；两腿共用 ref "r"。
	if err := l.Transfer(1, 2, "USDT", amt("USDT", 100), "trade", "r"); err != nil {
		t.Fatal(err)
	}
	if err := l.Transfer(2, 1, "BTC", amt("BTC", 1), "trade", "r"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 9900, 0)
	assertBalance(t, l, 1, "BTC", 1, 0)
	assertBalance(t, l, 2, "USDT", 100, 0)
	assertBalance(t, l, 2, "BTC", 9, 0)
}

// TestTransferDistinctRefsApply 不同 ref 的转账各自结算，互不影响。
func TestTransferDistinctRefsApply(t *testing.T) {
	l := ledger.New()
	_ = l.Deposit(1, "USDT", amt("USDT", 1000), "seed1")
	_ = l.Deposit(2, "USDT", amt("USDT", 0), "seed2")

	if err := l.Transfer(1, 2, "USDT", amt("USDT", 100), "x", "a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Transfer(1, 2, "USDT", amt("USDT", 50), "x", "b"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 850, 0)
	assertBalance(t, l, 2, "USDT", 150, 0)
}
