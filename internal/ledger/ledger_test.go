package ledger_test

import (
	"math"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/ledger"
)

// assertBalance 检查可用/冻结余额。账户不存在且期望均为 0 时视为通过
// （例如从未动用的坏账账户），其余情况账户不存在即报错。
func assertBalance(t *testing.T, l *ledger.Ledger, uid int64, asset string, avail, frozen float64) {
	t.Helper()
	a, f, ok := l.Balance(uid, asset)
	if !ok {
		if approx(avail, 0) == 0 && approx(frozen, 0) == 0 {
			return
		}
		t.Fatalf("account %d:%s not found (want avail %.8f frozen %.8f)", uid, asset, avail, frozen)
	}
	if approx(a, avail) != 0 || approx(f, frozen) != 0 {
		t.Fatalf("user %d balance = avail %.8f frozen %.8f, want avail %.8f frozen %.8f",
			uid, a, f, avail, frozen)
	}
}

// approx 浮点容差比较。
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
	if err := l.Deposit(1, "USDT", 10000, "seed"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 10000, 0)

	// 开仓冻结 5000 保证金
	if err := l.Freeze(1, "USDT", 5000); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 5000, 5000)

	// 余额不足时拒绝
	if err := l.Freeze(1, "USDT", 99999); err == nil {
		t.Fatal("expected insufficient balance error")
	}

	// 平仓释放
	if err := l.Unfreeze(1, "USDT", 5000); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 1, "USDT", 10000, 0)
}

// TestFundingClosedLoop 资金费多空转账闭环：净额恒为零。
func TestFundingClosedLoop(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	// 多头 user1、空头 user2，各充值 10000
	l.Deposit(1, asset, 10000, "seed")
	l.Deposit(2, asset, 10000, "seed")

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
			if err := l.Transfer(p.UserID, ledger.SysFundingPool, asset, -p.Payment, "funding", ref); err != nil {
				t.Fatal(err)
			}
		case p.Payment > 0:
			if err := l.Transfer(ledger.SysFundingPool, p.UserID, asset, p.Payment, "funding", ref); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 多头应少 5，空头应多 5
	assertBalance(t, l, 1, asset, 9995, 0)
	assertBalance(t, l, 2, asset, 10005, 0)
	// 中转池净额应为 0
	poolAvail, _, _ := l.Balance(ledger.SysFundingPool, asset)
	if approx(poolAvail, 0) != 0 {
		t.Fatalf("funding pool should be 0, got %.8f", poolAvail)
	}
}

// TestLiquidationForfeit 强平没收保证金入保险基金。
func TestLiquidationForfeit(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	l.Deposit(1, asset, 10000, "seed")
	l.Freeze(1, asset, 5000) // 开仓锁 5000

	// 强平：没收冻结保证金到保险基金，账户清零（演示简化）
	margin := 5000.0
	if err := l.Unfreeze(1, asset, margin); err != nil {
		t.Fatal(err)
	}
	if err := l.DebitAvailable(1, asset, margin, "liquidation", "liq:1"); err != nil {
		t.Fatal(err)
	}
	if err := l.CreditAvailable(ledger.SysInsurance, asset, margin, "liquidation", "liq:1"); err != nil {
		t.Fatal(err)
	}

	assertBalance(t, l, 1, asset, 5000, 0)                   // 剩余可用
	assertBalance(t, l, ledger.SysInsurance, asset, 5000, 0) // 保险基金 +5000

	// 强平业务借贷恒等：用户1 -5000 与 保险基金 +5000 相抵
	var liqSum float64
	for _, e := range l.Log() {
		if e.BizType == "liquidation" {
			liqSum += e.Delta
		}
	}
	if approx(liqSum, 0) != 0 {
		t.Fatalf("liquidation debit/credit must net to 0, got %.8f", liqSum)
	}
}

// TestReceiveOnChain 链上充值入账：用户可用 +amount，链上清算负债账户 -amount，
// chain_deposit 业务借贷恒等（净额 0）。
func TestReceiveOnChain(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	tx := "0xabc123"
	if err := l.ReceiveOnChain(7777, asset, 5000, tx); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, 7777, asset, 5000, 0)               // 用户入账
	assertBalance(t, l, ledger.SysChainClearing, asset, -5000, 0) // 清算负债 -5000

	var sum float64
	for _, e := range l.Log() {
		if e.BizType == "chain_deposit" {
			sum += e.Delta
		}
	}
	if approx(sum, 0) != 0 {
		t.Fatalf("chain_deposit debit/credit must net to 0, got %.8f", sum)
	}
}

// TestSettleWithdraw 链上提现清算：提现额+手续费从冻结余额原子划出，
// 贷记链上清算负债账户（余额回升），chain_withdraw 业务借贷恒等（净额 0）。
func TestSettleWithdraw(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7777)
	l.Deposit(uid, asset, 10000, "seed")
	// 提交提现：冻结 1000 + 2 手续费（走独立的提现冻结 WithdrawFrozen）
	if err := l.FreezeWithdraw(uid, asset, 1002); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8998, 0) // 可用减少，持仓冻结不变
	wf, _ := l.WithdrawFrozenBalance(uid, asset)
	if approx(wf, 1002) != 0 {
		t.Fatalf("withdraw frozen expect 1002, got %.8f", wf)
	}

	// 链上确认达标，结算划出
	if err := l.SettleWithdraw(uid, asset, 1000, 2, "0xwtx1"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8998, 0) // 提现冻结清零，可用不变（钱已离开系统）
	wf, _ = l.WithdrawFrozenBalance(uid, asset)
	if approx(wf, 0) != 0 {
		t.Fatalf("withdraw frozen expect 0 after settle, got %.8f", wf)
	}
	// SysChainClearing 从 0 回升到 +1002（对用户负债减少）
	assertBalance(t, l, ledger.SysChainClearing, asset, 1002, 0)

	var sum float64
	for _, e := range l.Log() {
		if e.BizType == "chain_withdraw" {
			sum += e.Delta
		}
	}
	if approx(sum, 0) != 0 {
		t.Fatalf("chain_withdraw debit/credit must net to 0, got %.8f", sum)
	}
}

// TestSettleWithdrawInsufficientFrozen 冻结不足时拒绝结算（防资损）。
func TestSettleWithdrawInsufficientFrozen(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7778)
	l.Deposit(uid, asset, 100, "seed")
	// 未冻结就结算，应报错
	if err := l.SettleWithdraw(uid, asset, 1000, 2, "0xwtx2"); err == nil {
		t.Fatal("expected insufficient frozen error")
	}
}

// TestWithdrawDepositClosedLoop 出入金闭环：先充值再提现，SysChainClearing 回到 0。
func TestWithdrawDepositClosedLoop(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7779)
	// 充值 5000 -> 清算负债 -5000
	l.ReceiveOnChain(uid, asset, 5000, "0xin")
	// 提现 5000（含费 0） -> 提现冻结 5000，结算后清算负债回到 0
	l.FreezeWithdraw(uid, asset, 5000)
	if err := l.SettleWithdraw(uid, asset, 5000, 0, "0xout"); err != nil {
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
	l.ReceiveOnChain(uid, asset, 4000, tx)
	assertBalance(t, l, uid, asset, 4000, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, -4000, 0)

	// 孤块丢弃，回拨（全额可用，无坏账）
	badDebt, err := l.ReverseOnChain(uid, asset, 4000, tx)
	if err != nil {
		t.Fatal(err)
	}
	if badDebt != 0 {
		t.Fatalf("expected zero bad debt, got %.8f", badDebt)
	}
	assertBalance(t, l, uid, asset, 0, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, 0, 0) // 负债回升归零
	assertBalance(t, l, ledger.SysBadDebt, asset, 0, 0)       // 无坏账

	var sum float64
	for _, e := range l.Log() {
		if e.BizType == "chain_rollback" {
			sum += e.Delta
		}
	}
	if approx(sum, 0) != 0 {
		t.Fatalf("chain_rollback debit/credit must net to 0, got %.8f", sum)
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
	l.ReceiveOnChain(uid, asset, 5000, tx)
	l.FreezeWithdraw(uid, asset, 3000)
	if err := l.SettleWithdraw(uid, asset, 3000, 0, "0xoutBD"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 2000, 0)            // 仅剩 2000 可用
	assertBalance(t, l, ledger.SysChainClearing, asset, -2000, 0)

	// 充值被孤块丢弃：只能回拨 2000，剩余 3000 为坏账
	badDebt, err := l.ReverseOnChain(uid, asset, 5000, tx)
	if err != nil {
		t.Fatal(err)
	}
	if approx(badDebt, 3000) != 0 {
		t.Fatalf("expected bad debt 3000, got %.8f", badDebt)
	}
	assertBalance(t, l, uid, asset, 0, 0)                   // 可用扣到 0
	assertBalance(t, l, ledger.SysChainClearing, asset, 3000, 0)  // 负债完全回升
	assertBalance(t, l, ledger.SysBadDebt, asset, -3000, 0)       // 交易所垫付坏账

	// 全局借贷恒等：全部账户净额之和应为 0
	var total float64
	for _, e := range l.Log() {
		total += e.Delta
	}
	if approx(total, 0) != 0 {
		t.Fatalf("global ledger must net to 0, got %.8f", total)
	}
}

// TestReverseWithdraw 提现孤块回滚：提现已清结算划出后若被重组丢弃，ReverseWithdraw
// 把资金从链上负债账户回拨到用户冻结，与 SettleWithdraw 互逆，借贷恒等。
func TestReverseWithdraw(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7783)
	l.Deposit(uid, asset, 10000, "seed")
	l.FreezeWithdraw(uid, asset, 1002) // 提现 1000 + 费 2（提现冻结）
	// 清结算划出
	if err := l.SettleWithdraw(uid, asset, 1000, 2, "0xwtxR"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8998, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, 1002, 0)
	wf, _ := l.WithdrawFrozenBalance(uid, asset)
	if approx(wf, 0) != 0 {
		t.Fatalf("withdraw frozen expect 0 after settle, got %.8f", wf)
	}

	// 提现被孤块重组：回拨到提现冻结（不影响持仓保证金 Frozen）
	if err := l.ReverseWithdraw(uid, asset, 1000, 2, "0xwtxR"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8998, 0)          // 持仓冻结不变
	wf, _ = l.WithdrawFrozenBalance(uid, asset)
	if approx(wf, 1002) != 0 {
		t.Fatalf("withdraw frozen expect 1002 after revert, got %.8f", wf)
	}
	assertBalance(t, l, ledger.SysChainClearing, asset, 0, 0) // 负债回到划出前

	// 退回可用（演示闭环）
	if err := l.UnfreezeWithdraw(uid, asset, 1002); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 10000, 0) // 用户余额完全恢复

	var sum float64
	for _, e := range l.Log() {
		if e.BizType == "chain_withdraw_revert" {
			sum += e.Delta
		}
	}
	if approx(sum, 0) != 0 {
		t.Fatalf("chain_withdraw_revert debit/credit must net to 0, got %.8f", sum)
	}
}

// TestDepositWithdrawReorgLoop 完整链上生命周期：充值 -> 提现 -> 充值的孤块回滚，
// 各阶段 SysChainClearing 守恒。
func TestDepositWithdrawReorgLoop(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7781)
	// 充值 5000
	l.ReceiveOnChain(uid, asset, 5000, "0xin3")
	assertBalance(t, l, ledger.SysChainClearing, asset, -5000, 0)
	// 提现 3000（含费 0）
	l.FreezeWithdraw(uid, asset, 3000)
	if err := l.SettleWithdraw(uid, asset, 3000, 0, "0xout3"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 2000, 0)
	assertBalance(t, l, ledger.SysChainClearing, asset, -2000, 0) // 负债随提现回升
	// 充值被孤块回滚：用户仅剩 2000 可用，3000 转坏账垫付
	badDebt, _ := l.ReverseOnChain(uid, asset, 5000, "0xin3")
	assertBalance(t, l, uid, asset, 0, 0)                    // 可用扣到 0
	assertBalance(t, l, ledger.SysChainClearing, asset, 3000, 0)  // 负债回升
	assertBalance(t, l, ledger.SysBadDebt, asset, -3000, 0)       // 坏账垫付
	if approx(badDebt, 3000) != 0 {
		t.Fatalf("expected bad debt 3000, got %.8f", badDebt)
	}
}

// TestBadDebtRecovery 坏账自动回收：充值被孤块回滚产生坏账后，用户后续充值会优先冲抵
// 交易所垫付坏账，剩余才入可用；坏账清零后 SysBadDebt 归零，借贷恒等。
func TestBadDebtRecovery(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7784)
	// 充值 5000 -> 提现动用 3000 -> 回滚产生坏账 3000
	l.ReceiveOnChain(uid, asset, 5000, "0xinR")
	l.FreezeWithdraw(uid, asset, 3000)
	if err := l.SettleWithdraw(uid, asset, 3000, 0, "0xoutR"); err != nil {
		t.Fatal(err)
	}
	l.ReverseOnChain(uid, asset, 5000, "0xinR")
	assertBalance(t, l, ledger.SysBadDebt, asset, -3000, 0)
	if approx(l.BadDebtTotal(asset), 3000) != 0 {
		t.Fatalf("bad debt total should be 3000, got %.8f", l.BadDebtTotal(asset))
	}

	// 后续充值 1000：全额冲抵坏账，用户可用仍为 0，坏账剩 2000
	if err := l.ReceiveOnChain(uid, asset, 1000, "0xinR2"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 0, 0)
	assertBalance(t, l, ledger.SysBadDebt, asset, -2000, 0)
	if approx(l.BadDebtTotal(asset), 2000) != 0 {
		t.Fatalf("bad debt total should be 2000, got %.8f", l.BadDebtTotal(asset))
	}

	// 再充值 2000：冲抵剩余 2000，用户可用 0，坏账归零
	if err := l.ReceiveOnChain(uid, asset, 2000, "0xinR3"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 0, 0)
	assertBalance(t, l, ledger.SysBadDebt, asset, 0, 0)
	if approx(l.BadDebtTotal(asset), 0) != 0 {
		t.Fatalf("bad debt should be cleared, got %.8f", l.BadDebtTotal(asset))
	}
}

// TestRepayBadDebt 主动补缴：用户用可用余额手动冲抵坏账，SysBadDebt 回升、用户可用减少，
// 借贷恒等；无坏账或余额不足时拒绝。
func TestRepayBadDebt(t *testing.T) {
	l := ledger.New()
	const asset = "USDT"
	uid := int64(7786)

	// 标准流程制造坏账 5000
	l.ReceiveOnChain(uid, asset, 5000, "0xA")
	l.FreezeWithdraw(uid, asset, 5000)
	l.SettleWithdraw(uid, asset, 5000, 0, "0xB")
	_, _ = l.ReverseOnChain(uid, asset, 5000, "0xA") // 坏账 5000
	assertBalance(t, l, ledger.SysBadDebt, asset, -5000, 0)

	// 余额不足负路径：此时用户可用为 0
	if err := l.RepayBadDebt(uid, asset, 100, "x"); err == nil {
		t.Fatal("repay with insufficient balance should fail")
	}
	// 无坏账用户负路径
	if err := l.RepayBadDebt(7787, asset, 100, "x"); err == nil {
		t.Fatal("repay with no bad debt should fail")
	}

	// 注入可用资金（Deposit 非链上充值，不触发自动回收）
	l.Deposit(uid, asset, 5000, "seed")
	assertBalance(t, l, uid, asset, 5000, 0)
	// 正向：补缴 2000 -> 坏账 3000，可用 3000
	if err := l.RepayBadDebt(uid, asset, 2000, "repay:7786"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, ledger.SysBadDebt, asset, -3000, 0)
	assertBalance(t, l, uid, asset, 3000, 0)
	// 再补缴 3000 -> 坏账清零，可用 0
	if err := l.RepayBadDebt(uid, asset, 3000, "repay:7786"); err != nil {
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
	l.ReceiveOnChain(uid, asset, 5000, "0xA")
	l.FreezeWithdraw(uid, asset, 3000)
	l.SettleWithdraw(uid, asset, 3000, 0, "0xB")
	_, _ = l.ReverseOnChain(uid, asset, 5000, "0xA")
	if !l.IsOutflowRestricted(uid, asset) {
		t.Fatal("user with bad debt should be outflow restricted")
	}
	// 充值回收 1000：坏账剩 2000，仍受限
	l.ReceiveOnChain(uid, asset, 1000, "0xC")
	if !l.IsOutflowRestricted(uid, asset) {
		t.Fatal("still bad debt, should remain restricted")
	}
	// 再充值 2000：坏账清零，解除限制
	l.ReceiveOnChain(uid, asset, 2000, "0xD")
	if l.IsOutflowRestricted(uid, asset) {
		t.Fatal("bad debt cleared, restriction should be lifted")
	}

	// 另一路径：造坏账后主动补缴清零也解除限制
	l2 := ledger.New()
	l2.ReceiveOnChain(uid, asset, 5000, "0xA")
	l2.FreezeWithdraw(uid, asset, 5000)
	l2.SettleWithdraw(uid, asset, 5000, 0, "0xB")
	_, _ = l2.ReverseOnChain(uid, asset, 5000, "0xA")
	if !l2.IsOutflowRestricted(uid, asset) {
		t.Fatal("bad debt should restrict")
	}
	l2.Deposit(uid, asset, 5000, "seed")
	if err := l2.RepayBadDebt(uid, asset, 5000, "repay"); err != nil {
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
	l.Deposit(uid, asset, 10000, "seed")

	// 开仓锁定保证金 2000（持仓冻结）
	if err := l.Freeze(uid, asset, 2000); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 8000, 2000) // available=8000, 持仓冻结=2000

	// 提交提现冻结 1000（提现冻结，独立于持仓冻结）
	if err := l.FreezeWithdraw(uid, asset, 1000); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 7000, 2000) // available=7000, 持仓冻结仍为 2000（未被提现冻结影响）
	wf, _ := l.WithdrawFrozenBalance(uid, asset)
	if approx(wf, 1000) != 0 {
		t.Fatalf("withdraw frozen expect 1000, got %.8f", wf)
	}

	// 提现结算划出：只扣提现冻结，持仓保证金必须保持不变
	if err := l.SettleWithdraw(uid, asset, 1000, 0, "0xw"); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 7000, 2000) // available/持仓冻结都不因提现改变
	wf, _ = l.WithdrawFrozenBalance(uid, asset)
	if approx(wf, 0) != 0 {
		t.Fatalf("withdraw frozen expect 0 after settle, got %.8f", wf)
	}

	// 持仓保证金释放（平仓）：只动 Frozen，不影响提现冻结（此时已为 0）
	if err := l.Unfreeze(uid, asset, 2000); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, l, uid, asset, 9000, 0)
	wf, _ = l.WithdrawFrozenBalance(uid, asset)
	if approx(wf, 0) != 0 {
		t.Fatalf("withdraw frozen must stay 0, got %.8f", wf)
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
	l.ReceiveOnChain(debtor, asset, 1000, "0xAA")
	l.Freeze(debtor, asset, 1000)
	l.SettleWithdraw(debtor, asset, 1000, 0, "0xBB")
	if _, err := l.ReverseOnChain(debtor, asset, 1000, "0xAA"); err != nil {
		t.Fatal(err)
	}
	if l.BadDebtTotal(asset) != 1000 {
		t.Fatalf("expect bad debt 1000, got %.2f", l.BadDebtTotal(asset))
	}

	// 种子其他用户可用余额（盈利用户，作分摊基数）
	if err := l.Deposit(alice, asset, 4000, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := l.Deposit(bob, asset, 6000, "seed"); err != nil {
		t.Fatal(err)
	}

	// 场景一：保险基金不足以覆盖（为 0），全部分摊 -> alice 摊 400，bob 摊 600
	detail, recovered, err := l.SocializeBadDebt(asset)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1000 {
		t.Fatalf("expect recovered 1000, got %.2f", recovered)
	}
	if l.BadDebtTotal(asset) != 0 {
		t.Fatalf("bad debt should be cleared, remain %.2f", l.BadDebtTotal(asset))
	}
	if l.IsOutflowRestricted(debtor, asset) {
		t.Fatal("bad debt cleared, debtor restriction should lift")
	}
	if got := detail[alice]; math.Abs(got-400) > 1e-6 {
		t.Fatalf("alice share expect 400, got %.2f", got)
	}
	if got := detail[bob]; math.Abs(got-600) > 1e-6 {
		t.Fatalf("bob share expect 600, got %.2f", got)
	}
	aliceBal, _, _ := l.Balance(alice, asset)
	bobBal, _, _ := l.Balance(bob, asset)
	if math.Abs(aliceBal-3600) > 1e-6 || math.Abs(bobBal-5400) > 1e-6 {
		t.Fatalf("unexpected balances alice=%.2f bob=%.2f", aliceBal, bobBal)
	}

	// 场景二：重新制造坏账，验证保险基金优先冲减
	l2 := ledger.New()
	l2.ReceiveOnChain(debtor, asset, 500, "0xAA")
	l2.Freeze(debtor, asset, 500)
	l2.SettleWithdraw(debtor, asset, 500, 0, "0xBB")
	if _, err := l2.ReverseOnChain(debtor, asset, 500, "0xAA"); err != nil {
		t.Fatal(err)
	}
	// 保险基金预先注资 300
	if err := l2.CreditAvailable(ledger.SysInsurance, asset, 300, "seed_ins", "seed"); err != nil {
		t.Fatal(err)
	}
	// 再给一个盈利用户 1000 作分摊基数
	if err := l2.Deposit(alice, asset, 1000, "seed"); err != nil {
		t.Fatal(err)
	}
	// 坏账 500 = 保险基金冲 300 + 社会化分摊 200（alice 全摊）
	d2, rec2, err := l2.SocializeBadDebt(asset)
	if err != nil {
		t.Fatal(err)
	}
	if rec2 != 500 {
		t.Fatalf("expect recovered 500, got %.2f", rec2)
	}
	if l2.BadDebtTotal(asset) != 0 {
		t.Fatalf("bad debt should clear, remain %.2f", l2.BadDebtTotal(asset))
	}
	if got := d2[alice]; math.Abs(got-200) > 1e-6 {
		t.Fatalf("alice socialized share expect 200, got %.2f", got)
	}
}

// TestReconcileProductionPaths 验证：全程使用生产资金路径（ReceiveOnChain/Freeze/FreezeWithdraw/
// SettleWithdraw/ReverseOnChain/ReverseWithdraw/SocializeBadDebt）时，复式记账恒等式恒成立——
// 每个资产下所有账户（含系统负债账户）权益总和为 0。这是交易所资金安全的零知识对账不变量。
func TestReconcileProductionPaths(t *testing.T) {
	l := ledger.New()
	asset := "USDT"

	// 用户1：链上充值 10000
	if err := l.ReceiveOnChain(1, asset, 10000, "0x1"); err != nil {
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after deposit should balance: %v", l.Reconcile())
	}

	// 用户1：提现受理 + 结算（资金离场）
	if err := l.FreezeWithdraw(1, asset, 1000); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleWithdraw(1, asset, 1000, 0, "0xw1"); err != nil {
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after settle withdraw should balance: %v", l.Reconcile())
	}

	// 用户2：充值后开仓冻结保证金（不影响平衡）
	if err := l.ReceiveOnChain(2, asset, 5000, "0x2"); err != nil {
		t.Fatal(err)
	}
	if err := l.Freeze(2, asset, 500); err != nil {
		t.Fatal(err)
	}
	if !l.IsBalanced() {
		t.Fatalf("after position freeze should balance: %v", l.Reconcile())
	}

	// 用户3：充值 -> 提现动用 -> 孤块回滚充值（造坏账，SysBadDebt 垫付）
	if err := l.ReceiveOnChain(3, asset, 5000, "0x3"); err != nil {
		t.Fatal(err)
	}
	if err := l.FreezeWithdraw(3, asset, 3000); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleWithdraw(3, asset, 3000, 0, "0xw3"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReverseOnChain(3, asset, 5000, "0x3"); err != nil { // 回滚整笔充值 -> 坏账 3000
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
	if err := l.FreezeWithdraw(1, asset, 2000); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleWithdraw(1, asset, 2000, 0, "0xw4"); err != nil {
		t.Fatal(err)
	}
	if err := l.ReverseWithdraw(1, asset, 2000, 0, "0xw4"); err != nil {
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
	if err := l.Deposit(1, asset, 1000, "seed"); err != nil { // 凭空铸币，未配对
		t.Fatal(err)
	}
	if l.IsBalanced() {
		t.Fatal("unpaired Deposit mint should break balance, but Reconcile reports balanced")
	}
	dev := l.Reconcile()
	if math.Abs(dev[asset]-1000) > 1e-6 {
		t.Fatalf("expect deviation 1000 for unpaired mint, got %.6f", dev[asset])
	}
}

// TestRunReconcileOnceUpdatesStats 验证 RunReconcileOnce 更新巡检快照并反映平衡态。
func TestRunReconcileOnceUpdatesStats(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	// 生产路径：充值经链上确认入账，借贷恒等 -> 平衡
	l.ReceiveOnChain(1, asset, 1000, "0xin")
	st := l.RunReconcileOnce()
	if !st.LastBalanced {
		t.Fatalf("production path should be balanced, got deviation %v", st.LastDeviation)
	}
	if st.LastRun.IsZero() {
		t.Fatal("LastRun should be set")
	}

	// 注入凭空铸币 -> 不平衡，偏差应被记录
	l.Deposit(2, asset, 500, "seed")
	st = l.RunReconcileOnce()
	if st.LastBalanced {
		t.Fatal("unpaired mint should break balance")
	}
	if math.Abs(st.LastDeviation[asset]-500) > 1e-6 {
		t.Fatalf("expect deviation 500, got %.6f", st.LastDeviation[asset])
	}
}

// TestReconcileAlertHookFiresOnImbalance 验证不平账时告警回调被触发（监控接入点）。
func TestReconcileAlertHookFiresOnImbalance(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	got := make(chan map[string]float64, 1)
	l.SetReconcileAlertHook(func(dev map[string]float64) {
		got <- dev
	})
	l.Deposit(1, asset, 777, "seed") // 凭空铸币触发不平衡
	l.RunReconcileOnce()

	select {
	case dev := <-got:
		if math.Abs(dev[asset]-777) > 1e-6 {
			t.Fatalf("alert hook deviation expect 777, got %.6f", dev[asset])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alert hook was not invoked on imbalance")
	}

	// 关闭 hook 后不再触发
	l.SetReconcileAlertHook(nil)
	l.Deposit(2, asset, 100, "seed")
	select {
	case <-got:
		t.Fatal("alert hook should not fire after being cleared")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestReconcilerGoroutineStartStop 验证后台巡检 goroutine 定时运行且可干净停止（幂等）。
func TestReconcilerGoroutineStartStop(t *testing.T) {
	l := ledger.New()
	l.SetReconcileAlertHook(func(dev map[string]float64) {})
	l.Deposit(1, "USDT", 100, "seed") // 持续不平衡，巡检会反复累加 ImbalanceCount

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
	l.ReceiveOnChain(A, asset, 5000, "0xAin")
	// 制造 B 坏账 2000：充值 8000 -> 提现动用 2000 -> 回滚（垫付 2000）
	l.ReceiveOnChain(B, asset, 8000, "0xBin")
	l.FreezeWithdraw(A, asset, 1000)
	l.SettleWithdraw(A, asset, 1000, 0, "0xAout")
	l.FreezeWithdraw(B, asset, 2000)
	l.SettleWithdraw(B, asset, 2000, 0, "0xBout")
	l.ReverseOnChain(A, asset, 5000, "0xAin")
	l.ReverseOnChain(B, asset, 8000, "0xBin")

	if !l.IsOutflowRestricted(A, asset) || !l.IsOutflowRestricted(B, asset) {
		t.Fatal("both debtors should be restricted after bad debt")
	}
	if got := l.BadDebtTotal(asset); math.Abs(got-3000) > 1e-6 {
		t.Fatalf("expect total bad debt 3000, got %.2f", got)
	}

	// 给 A/B 可用余额（演示用 CreditAvailable 不触发自动回收，仅用于模拟后续补缴资金）
	l.CreditAvailable(A, asset, 5000, "topup", "topup")
	l.CreditAvailable(B, asset, 5000, "topup", "topup")

	// A 仅补缴自身 1000 -> 仅 A 解限，B 仍受限
	if err := l.RepayBadDebt(A, asset, 1000, "repayA"); err != nil {
		t.Fatal(err)
	}
	if l.IsOutflowRestricted(A, asset) {
		t.Fatal("A repaid own debt, should be lifted")
	}
	if !l.IsOutflowRestricted(B, asset) {
		t.Fatal("B still owes, must remain restricted (no false lift)")
	}
	if got := l.BadDebtTotal(asset); math.Abs(got-2000) > 1e-6 {
		t.Fatalf("expect remaining bad debt 2000, got %.2f", got)
	}

	// B 补缴 2000 -> 全局结清，全部解限
	if err := l.RepayBadDebt(B, asset, 2000, "repayB"); err != nil {
		t.Fatal(err)
	}
	if l.IsOutflowRestricted(A, asset) || l.IsOutflowRestricted(B, asset) {
		t.Fatal("all debt cleared, both should be lifted")
	}
	if got := l.BadDebtTotal(asset); got > 1e-6 {
		t.Fatalf("expect zero bad debt, got %.2f", got)
	}
}

// TestVoluntaryRepayCoversOthers 验证用户自愿多缴可替其他债务人兜底：A 多缴覆盖 B 的欠款，
// 全局结清后两人皆解限。
func TestVoluntaryRepayCoversOthers(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	const A, B int64 = 7201, 7202
	l.ReceiveOnChain(A, asset, 5000, "0xAin")
	l.ReceiveOnChain(B, asset, 8000, "0xBin")
	l.FreezeWithdraw(A, asset, 1000)
	l.SettleWithdraw(A, asset, 1000, 0, "0xAout")
	l.FreezeWithdraw(B, asset, 2000)
	l.SettleWithdraw(B, asset, 2000, 0, "0xBout")
	l.ReverseOnChain(A, asset, 5000, "0xAin")
	l.ReverseOnChain(B, asset, 8000, "0xBin")
	l.CreditAvailable(A, asset, 5000, "topup", "topup")

	// A 缴 3000（自身 1000 + 替 B 兜底 2000）-> 全局结清，全部解限
	if err := l.RepayBadDebt(A, asset, 3000, "repayA"); err != nil {
		t.Fatal(err)
	}
	if l.IsOutflowRestricted(A, asset) || l.IsOutflowRestricted(B, asset) {
		t.Fatal("A covered both, both should be lifted")
	}
	if got := l.BadDebtTotal(asset); got > 1e-6 {
		t.Fatalf("expect zero bad debt, got %.2f", got)
	}
}

// TestSocializeGovernance 验证社会化分摊治理审批流：propose 仅预览不动账本，approve 凭
// 提案号执行；坏账来源方（受限）不参与分摊，仅非受限盈利用户按比例承担。
func TestSocializeGovernance(t *testing.T) {
	l := ledger.New()
	asset := "USDT"
	const debtor, p1, p2 int64 = 7301, 7302, 7303

	// 制造坏账 1000（先充值再回滚）
	l.ReceiveOnChain(debtor, asset, 5000, "0xdin")
	l.FreezeWithdraw(debtor, asset, 1000)
	l.SettleWithdraw(debtor, asset, 1000, 0, "0xdout")
	l.ReverseOnChain(debtor, asset, 5000, "0xdin")
	if l.BadDebtTotal(asset) < 999 {
		t.Fatalf("expect bad debt ~1000, got %.2f", l.BadDebtTotal(asset))
	}

	// 非受限盈利用户
	l.CreditAvailable(p1, asset, 4000, "seed", "seed")
	l.CreditAvailable(p2, asset, 6000, "seed", "seed")

	// 无坏账时 propose 报错（反向）
	if _, _, err := l.ProposeSocialize("BTC"); err == nil {
		t.Fatal("propose on no bad debt should error")
	}

	// propose：仅预览，不动账本
	id, preview, err := l.ProposeSocialize(asset)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Status != "pending" || preview.Recovered < 999 {
		t.Fatalf("preview unexpected: %+v", preview)
	}
	if _, ok := preview.Detail[debtor]; ok {
		t.Fatal("debtor (restricted) must NOT be in socialize shares")
	}
	if got := preview.Detail[p1]; math.Abs(got-400) > 1e-6 {
		t.Fatalf("p1 share expect 400, got %.2f", got)
	}
	if got := preview.Detail[p2]; math.Abs(got-600) > 1e-6 {
		t.Fatalf("p2 share expect 600, got %.2f", got)
	}
	// propose 后账本未变（坏账仍在、限制仍在）
	if l.BadDebtTotal(asset) < 999 {
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
	if math.Abs(recovered-1000) > 1e-6 {
		t.Fatalf("recovered expect 1000, got %.2f", recovered)
	}
	if l.BadDebtTotal(asset) > 1e-6 {
		t.Fatalf("bad debt should be cleared, got %.2f", l.BadDebtTotal(asset))
	}
	if l.IsOutflowRestricted(debtor, asset) {
		t.Fatal("debtor should be lifted after socialize clears bad debt")
	}
	if got := detail[p1]; math.Abs(got-400) > 1e-6 {
		t.Fatalf("p1 share expect 400, got %.2f", got)
	}
}

// TestSnapshotRestore 验证账本持久化：构造余额/坏账/出金限制/治理提案后，快照到新账本，
// 恢复后资金安全状态（可用/冻结/提现冻结、坏账归属、出金限制、提案、对账平衡）完全一致。
func TestSnapshotRestore(t *testing.T) {
	l := ledger.New()
	asset := "USDT"

	// 余额：可用/持仓冻结/提现冻结三种形态都造值，验证快照覆盖全部字段。
	_ = l.Deposit(1, asset, 50000, "seed")
	_ = l.Freeze(1, asset, 8000)         // 持仓保证金冻结
	_ = l.FreezeWithdraw(1, asset, 2000) // 提现冻结
	// 坏账来源方：可用余额较少，充值回滚时将产生交易所垫付坏账。
	_ = l.Deposit(2, asset, 10000, "seed")

	// 坏账闭环：充值回滚制造坏账（用户可用不足以全额回拨）+ 出金限制 + 坏账归属。
	bd, err := l.ReverseOnChain(2, asset, 30000, "txbad")
	if err != nil {
		t.Fatal(err)
	}
	if approx(bd, 20000) != 0 {
		t.Fatalf("expect bad debt 20000, got %.2f", bd)
	}
	if !l.IsOutflowRestricted(2, asset) {
		t.Fatal("debtor should be restricted after bad debt")
	}
	if approx(l.BadDebtTotal(asset), 20000) != 0 {
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
	if wf, ok := l2.WithdrawFrozenBalance(1, asset); !ok || approx(wf, 2000) != 0 {
		t.Fatalf("user1 withdraw frozen = %.8f, want 2000", wf)
	}
	assertBalance(t, l2, 2, asset, 0, 0) // 坏账回滚已扣减可用至 0

	// 2) 坏账归属与出金限制恢复一致
	if !l2.IsOutflowRestricted(2, asset) {
		t.Fatal("restriction should survive restore")
	}
	if approx(l2.BadDebtTotal(asset), 20000) != 0 {
		t.Fatalf("bad debt total mismatch after restore: %.2f", l2.BadDebtTotal(asset))
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
	if approx(dev1[asset], dev2[asset]) != 0 {
		t.Fatalf("reconcile deviation mismatch after restore: %.8f vs %.8f", dev1[asset], dev2[asset])
	}

	// 5) 恢复后坏账回收闭环依然可用：用户2链上充值回填应冲抵归属并解限。
	if err := l2.ReceiveOnChain(2, asset, 30000, "txrepay"); err != nil {
		t.Fatal(err)
	}
	if l2.IsOutflowRestricted(2, asset) {
		t.Fatal("debtor restriction should lift after repayment via restored ledger")
	}
	if l2.BadDebtTotal(asset) > 1e-6 {
		t.Fatalf("bad debt should be cleared after repay, got %.2f", l2.BadDebtTotal(asset))
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
	_ = l.ReceiveOnChain(1, asset, 100000, "tx1")
	if approx(l.HotWalletBalance(asset), 100000) != 0 {
		t.Fatalf("hot wallet = %.2f, want 100000", l.HotWalletBalance(asset))
	}
	if approx(l.InventoryMatchesLiability(asset), 0) != 0 {
		t.Fatalf("inventory mismatch = %.8f, want 0", l.InventoryMatchesLiability(asset))
	}

	// 设热钱包上限 50000：此前 100k 已超限，设置即收敛——自动归集 50000 到冷钱包。
	l.SetHotWalletCap(asset, 50000)
	if approx(l.HotWalletBalance(asset), 50000) != 0 {
		t.Fatalf("hot wallet after cap reconcile = %.2f, want 50000", l.HotWalletBalance(asset))
	}
	if approx(l.ColdWalletBalance(asset), 50000) != 0 {
		t.Fatalf("cold wallet after auto-sweep = %.2f, want 50000", l.ColdWalletBalance(asset))
	}
	if approx(l.HotWalletExcess(asset), 0) != 0 {
		t.Fatalf("hot excess should be 0 after auto-sweep, got %.2f", l.HotWalletExcess(asset))
	}

	// 用户1提现 30000（含 500 手续费）：热钱包减少 total，冷钱包不变，不变量仍成立。
	if err := l.FreezeWithdraw(1, asset, 30000); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleWithdraw(1, asset, 29500, 500, "txw1"); err != nil {
		t.Fatal(err)
	}
	if approx(l.HotWalletBalance(asset), 20000) != 0 {
		t.Fatalf("hot wallet after withdraw = %.2f, want 20000", l.HotWalletBalance(asset))
	}
	if approx(l.InventoryMatchesLiability(asset), 0) != 0 {
		t.Fatalf("inventory mismatch after withdraw = %.8f", l.InventoryMatchesLiability(asset))
	}

	// 手工归集 15000 到冷钱包，再回调 5000 回热钱包：链上总额与负债不变（内部转账）。
	if swept, err := l.SweepToCold(asset, 15000); err != nil || approx(swept, 15000) != 0 {
		t.Fatalf("sweep failed: swept=%.2f err=%v", swept, err)
	}
	if approx(l.HotWalletBalance(asset), 5000) != 0 || approx(l.ColdWalletBalance(asset), 65000) != 0 {
		t.Fatalf("after sweep hot=%.2f cold=%.2f, want 5000/65000", l.HotWalletBalance(asset), l.ColdWalletBalance(asset))
	}
	if moved, err := l.UnsweepFromCold(asset, 5000); err != nil || approx(moved, 5000) != 0 {
		t.Fatalf("unsweep failed: moved=%.2f err=%v", moved, err)
	}
	if approx(l.HotWalletBalance(asset), 10000) != 0 || approx(l.ColdWalletBalance(asset), 60000) != 0 {
		t.Fatalf("after unsweep hot=%.2f cold=%.2f, want 10000/60000", l.HotWalletBalance(asset), l.ColdWalletBalance(asset))
	}
	if approx(l.HotWalletBalance(asset)+l.ColdWalletBalance(asset), 70000) != 0 {
		t.Fatalf("total on-chain should stay 70000, got %.2f", l.HotWalletBalance(asset)+l.ColdWalletBalance(asset))
	}

	// 孤块回滚：充值被丢弃 → 热钱包对称回拨 100000（从未真正收到），不变量仍成立。
	if _, err := l.ReverseOnChain(1, asset, 100000, "tx1"); err != nil {
		t.Fatal(err)
	}
	// hot: 10000 - 100000 = -90000；cold 60000；总额 -30000 == -SysChainClearing。
	if approx(l.HotWalletBalance(asset), -90000) != 0 {
		t.Fatalf("hot wallet after rollback = %.2f, want -90000 (never received)", l.HotWalletBalance(asset))
	}
	if approx(l.InventoryMatchesLiability(asset), 0) != 0 {
		t.Fatalf("inventory mismatch after rollback = %.8f", l.InventoryMatchesLiability(asset))
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
	l.SetDailyWithdrawLimit(asset, 15000)

	uid := int64(9001)
	_ = l.Deposit(uid, asset, 100000, "seed")
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
			v = 0
		}
		if approx(v, want) != 0 {
			t.Fatalf("user %d withdraw frozen = %.8f, want %.8f", uid, v, want)
		}
	}

	// 1) 受理进入冷静期：资金冻结但未离链（Available 扣减，WithdrawFrozen 增加）。
	id, holdUntil, err := l.RequestWithdrawHold(uid, asset, 10000, 0, "ETH", "0xout")
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
	if _, _, e := l.RequestWithdrawHold(uid, asset, 5000, 0, "ETH", "0xout"); e == nil {
		t.Fatal("request during global freeze should fail")
	}
	l.SetGlobalWithdrawalFreeze(false)

	// 4) 每日限额：累计 10000 OK，再 10000 超 15000 被拒；撤销后可再次受理。
	id1, _, err := l.RequestWithdrawHold(uid, asset, 10000, 0, "ETH", "0xout")
	if err != nil {
		t.Fatal(err)
	}
	wf(10000)
	if _, _, e := l.RequestWithdrawHold(uid, asset, 10000, 0, "ETH", "0xout"); e == nil {
		t.Fatal("second request exceeding daily limit should fail")
	}
	if err := l.CancelWithdrawHold(id1); err != nil {
		t.Fatal(err)
	}
	wf(0)
	id2, _, err := l.RequestWithdrawHold(uid, asset, 10000, 0, "ETH", "0xout")
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
	l.SetDailyWithdrawLimit(asset, 1e9)              // 放大日限额，聚焦白名单逻辑

	uid := int64(9101)
	_ = l.Deposit(uid, asset, 100000, "seed")

	// 1) 预登记地址（默认未验证）。
	addr, err := l.AddWithdrawAddress(uid, asset, "ETH", "0xsafe", "main")
	if err != nil {
		t.Fatal(err)
	}
	if addr.Verified {
		t.Fatal("newly registered address should be unverified")
	}
	// 未验证：提现被拒。
	if _, _, e := l.RequestWithdrawHold(uid, asset, 1000, 0, "ETH", "0xsafe"); e == nil {
		t.Fatal("unverified address should be rejected")
	}

	// 2) 验证后仍在验证冷静期内：仍被拒。
	if err := l.ConfirmWithdrawAddress(uid, asset, "ETH", "0xsafe"); err != nil {
		t.Fatal(err)
	}
	if _, _, e := l.RequestWithdrawHold(uid, asset, 1000, 0, "ETH", "0xsafe"); e == nil {
		t.Fatal("address within verify cooldown should be rejected")
	}

	// 3) 等待验证冷静期过后：地址可用，进入提现冷静期并清算成功。
	time.Sleep(150 * time.Millisecond)
	id, _, err := l.RequestWithdrawHold(uid, asset, 1000, 0, "ETH", "0xsafe")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // 等提现冷静期
	if _, err := l.FinalizeWithdrawHold(id); err != nil {
		t.Fatal(err)
	}

	// 4) 未登记地址提现被拒；重复登记报错；验证不存在地址报错。
	if _, _, e := l.RequestWithdrawHold(uid, asset, 1000, 0, "ETH", "0xevil"); e == nil {
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
	if _, _, e := l.RequestWithdrawHold(uid, asset, 1000, 0, "ETH", "0xsafe"); e == nil {
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
	_ = l.Deposit(uid, asset, 100000, "seed")
	_, _ = l.AddWithdrawAddress(uid, asset, "ETH", "0xsafe", "main")
	_ = l.ConfirmWithdrawAddress(uid, asset, "ETH", "0xsafe")

	// 前两次提现正常受理（次数=1,2 < 3）。
	if _, _, err := l.RequestWithdrawHold(uid, asset, 100, 0, "ETH", "0xsafe"); err != nil {
		t.Fatalf("1st withdraw should pass: %v", err)
	}
	if _, _, err := l.RequestWithdrawHold(uid, asset, 100, 0, "ETH", "0xsafe"); err != nil {
		t.Fatalf("2nd withdraw should pass: %v", err)
	}
	// 第三次：次数达阈值，触发自动全局冻结并拒绝本次受理。
	if _, _, err := l.RequestWithdrawHold(uid, asset, 100, 0, "ETH", "0xsafe"); err == nil {
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
	if _, _, err := l.RequestWithdrawHold(uid, asset, 100, 0, "ETH", "0xsafe"); err == nil {
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
	_ = l2.Deposit(uid2, asset, 100000, "seed")
	_, _ = l2.AddWithdrawAddress(uid2, asset, "ETH", "0xsafe", "main")
	_ = l2.ConfirmWithdrawAddress(uid2, asset, "ETH", "0xsafe")
	if _, _, err := l2.RequestWithdrawHold(uid2, asset, 100, 0, "ETH", "0xsafe"); err != nil {
		t.Fatalf("1st (100) should pass: %v", err)
	}
	if _, _, err := l2.RequestWithdrawHold(uid2, asset, 100, 0, "ETH", "0xsafe"); err != nil {
		t.Fatalf("2nd (sum 200) should pass: %v", err)
	}
	if _, _, err := l2.RequestWithdrawHold(uid2, asset, 100, 0, "ETH", "0xsafe"); err == nil {
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
	_ = l4.Deposit(uid4, asset, 100000, "seed")
	_, _ = l4.AddWithdrawAddress(uid4, asset, "ETH", "0xsafe", "main")
	_ = l4.ConfirmWithdrawAddress(uid4, asset, "ETH", "0xsafe")
	_, _, _ = l4.RequestWithdrawHold(uid4, asset, 100, 0, "ETH", "0xsafe")
	_, _, _ = l4.RequestWithdrawHold(uid4, asset, 100, 0, "ETH", "0xsafe") // 2次触发
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
