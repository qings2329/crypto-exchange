package settlement

import (
	"context"
	"testing"
	"time"
)

func approx(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// 提交充值后经 Required 个区块确认，状态转为 Credited。
func TestMockGatewayConfirmation(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour) // 大间隔，手动 Tick
	g.Start()
	defer g.Stop()

	ev, err := g.SubmitDeposit(7001, "USDT", ChainETH, 5000, "")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	if ev.Status != DepositPending || ev.Confirmations != 0 {
		t.Fatalf("initial status wrong: %+v", ev)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := g.Watch(ctx)

	// 推进 2 个区块
	g.Tick()
	if p := g.Pending(); p[0].Status != DepositPending || p[0].Confirmations != 1 {
		t.Fatalf("after 1 tick want confirmations=1 pending, got %+v", p[0])
	}
	g.Tick()

	// 应已 Credited 并经 Watch 推送
	select {
	case got := <-ch:
		if got.Status != DepositCredited || got.TxHash != ev.TxHash {
			t.Fatalf("watch event wrong: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watch did not receive credited event")
	}
	if p := g.Pending(); p[0].Status != DepositCredited || p[0].Confirmations != 2 {
		t.Fatalf("after 2 ticks want credited, got %+v", p[0])
	}
}

// Required 确认数必须严格达到才入账。
func TestMockGatewayRequiredBlocks(t *testing.T) {
	g := NewMockChainGateway(3, time.Hour)
	g.Start()
	defer g.Stop()
	_, _ = g.SubmitDeposit(7002, "USDT", ChainBTC, 1000, "addr1")
	g.Tick()
	g.Tick()
	if p := g.Pending(); p[0].Status != DepositPending {
		t.Fatalf("after 2 ticks (required 3) should still be pending, got %+v", p[0])
	}
	g.Tick()
	if p := g.Pending(); p[0].Status != DepositCredited {
		t.Fatalf("after 3 ticks should be credited, got %+v", p[0])
	}
}

// 幂等键与地址生成确定性。
func TestGenerateHelpers(t *testing.T) {
	a1 := GenerateAddress(1, ChainETH)
	a2 := GenerateAddress(1, ChainETH)
	if a1 != a2 {
		t.Fatalf("address should be deterministic: %s vs %s", a1, a2)
	}
	if len(a1) == 0 || a1[:3] != "ETH" {
		t.Fatalf("address should be prefixed by chain: %s", a1)
	}
	tx1 := GenerateTxHash(1, "USDT", ChainETH, 5000, 1)
	tx2 := GenerateTxHash(1, "USDT", ChainETH, 5000, 2)
	if tx1 == tx2 {
		t.Fatalf("different nonce should yield different tx hash")
	}
}

// 非法参数拒绝。
func TestSubmitValidation(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour)
	if _, err := g.SubmitDeposit(0, "USDT", ChainETH, 100, ""); err == nil {
		t.Fatalf("zero user should be rejected")
	}
	if _, err := g.SubmitDeposit(1, "USDT", ChainETH, 0, ""); err == nil {
		t.Fatalf("zero amount should be rejected")
	}
}

// 提现经 Required 个区块确认后状态转为 Credited 并经 Watch 推送。
func TestMockWithdrawGatewaySuccess(t *testing.T) {
	g := NewMockWithdrawGateway(2, time.Hour)
	g.Start()
	defer g.Stop()

	ev, err := g.SubmitWithdraw(8001, "USDT", ChainETH, 1000, 2, "", false)
	if err != nil {
		t.Fatalf("submit withdraw failed: %v", err)
	}
	if ev.Status != WithdrawPending || ev.Confirmations != 0 {
		t.Fatalf("initial withdraw status wrong: %+v", ev)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := g.WatchWithdraw(ctx)

	g.Tick() // Pending -> Broadcasting, confirmations=1
	if p := g.WithdrawHistory(); p[0].Status != WithdrawBroadcasting || p[0].Confirmations != 1 {
		t.Fatalf("after 1 tick want broadcasting conf=1, got %+v", p[0])
	}
	g.Tick() // Broadcasting -> Credited

	select {
	case got := <-ch:
		if got.Status != WithdrawCredited || got.TxHash != ev.TxHash {
			t.Fatalf("watch event wrong: %+v", got)
		}
		if !approx(got.Amount, 1000, 1e-9) || !approx(got.Fee, 2, 1e-9) {
			t.Fatalf("amount/fee mismatch: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watch did not receive credited event")
	}
	if p := g.WithdrawHistory(); p[0].Status != WithdrawCredited || p[0].Confirmations != 2 {
		t.Fatalf("after 2 ticks want credited, got %+v", p[0])
	}
}

// 注入失败时提现首区块直接转 Failed 并经 Watch 推送。
func TestMockWithdrawGatewayFailure(t *testing.T) {
	g := NewMockWithdrawGateway(2, time.Hour)
	g.Start()
	defer g.Stop()

	_, err := g.SubmitWithdraw(8002, "USDT", ChainTRON, 500, 1, "Txxx", true)
	if err != nil {
		t.Fatalf("submit withdraw failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := g.WatchWithdraw(ctx)

	g.Tick() // WillFail -> Failed

	select {
	case got := <-ch:
		if got.Status != WithdrawFailed {
			t.Fatalf("want failed, got %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watch did not receive failed event")
	}
	if p := g.WithdrawHistory(); p[0].Status != WithdrawFailed {
		t.Fatalf("want failed status, got %+v", p[0])
	}
}

// 已清结算（Credited）的提现经 WithdrawReorg 标记 orphaned，并经 WatchWithdrawRollback 推送。
func TestMockWithdrawGatewayReorg(t *testing.T) {
	g := NewMockWithdrawGateway(2, time.Hour)
	g.Start()
	defer g.Stop()

	ev, err := g.SubmitWithdraw(8003, "USDT", ChainETH, 700, 1, "0xaddr3", false)
	if err != nil {
		t.Fatalf("submit withdraw failed: %v", err)
	}
	g.Tick() // Pending -> Broadcasting
	g.Tick() // Broadcasting -> Credited

	if p := g.WithdrawHistory(); p[0].Status != WithdrawCredited {
		t.Fatalf("want credited before reorg, got %+v", p[0])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rch, _ := g.WatchWithdrawRollback(ctx)

	got, err := g.WithdrawReorg(ev.TxHash)
	if err != nil {
		t.Fatalf("reorg failed: %v", err)
	}
	if got.Status != WithdrawOrphaned {
		t.Fatalf("want orphaned, got %+v", got)
	}

	select {
	case rv := <-rch:
		if rv.Status != WithdrawOrphaned || rv.TxHash != ev.TxHash {
			t.Fatalf("rollback event mismatch: %+v", rv)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watch rollback did not receive event")
	}
	if p := g.WithdrawHistory(); p[0].Status != WithdrawOrphaned {
		t.Fatalf("want orphaned in history, got %+v", p[0])
	}
}

// 未达 Credited 的提现不可重组（状态校验）。
func TestMockWithdrawGatewayReorgGuard(t *testing.T) {
	g := NewMockWithdrawGateway(2, time.Hour)
	g.Start()
	defer g.Stop()
	ev, _ := g.SubmitWithdraw(8004, "USDT", ChainETH, 100, 0, "0xaddr4", false)
	if _, err := g.WithdrawReorg(ev.TxHash); err == nil {
		t.Fatalf("reorg before credited should be rejected")
	}
}

// 提现非法参数拒绝。
func TestWithdrawValidation(t *testing.T) {
	g := NewMockWithdrawGateway(2, time.Hour)
	if _, err := g.SubmitWithdraw(0, "USDT", ChainETH, 100, 0, "", false); err == nil {
		t.Fatalf("zero user should be rejected")
	}
	if _, err := g.SubmitWithdraw(1, "USDT", ChainETH, 0, 0, "", false); err == nil {
		t.Fatalf("zero amount should be rejected")
	}
}

// 孤块/重组：已确认充值经 Reorg 转 orphaned 并经 WatchRollback 推送。
func TestMockGatewayReorg(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour)
	g.Start()
	defer g.Stop()

	ev, err := g.SubmitDeposit(7101, "USDT", ChainETH, 3000, "")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	g.Tick()
	g.Tick() // 达 2 确认 -> Credited
	if p := g.Pending(); p[0].Status != DepositCredited {
		t.Fatalf("want credited before reorg, got %+v", p[0])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rch, _ := g.WatchRollback(ctx)

	if _, err := g.Reorg(ev.TxHash); err != nil {
		t.Fatalf("reorg failed: %v", err)
	}
	select {
	case got := <-rch:
		if got.Status != DepositOrphaned || got.TxHash != ev.TxHash {
			t.Fatalf("rollback event wrong: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watch did not receive rollback event")
	}
	if p := g.Pending(); p[0].Status != DepositOrphaned {
		t.Fatalf("after reorg want orphaned, got %+v", p[0])
	}
}

// Reorg 未知交易哈希报错。
func TestReorgUnknownTx(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour)
	if _, err := g.Reorg("0xdeadbeef"); err == nil {
		t.Fatalf("unknown tx should be rejected")
	}
}

// 充值重组窗口：未达最终确认（Pending）的充值被重组应安全回退（重置确认数、不推送回滚、
// 不触发坏账）；已达最终确认（Credited）的充值被重组才触发回滚。
func TestDepositReorgWindow(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour)
	g.Start()
	defer g.Stop()

	ev, err := g.SubmitDeposit(7201, "USDT", ChainETH, 3000, "")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rch, _ := g.WatchRollback(ctx)

	// 未达最终确认即重组：安全回退，不应推送回滚事件
	if _, err := g.Reorg(ev.TxHash); err != nil {
		t.Fatalf("pending reorg should succeed: %v", err)
	}
	if p := g.Pending(); p[0].Status != DepositPending || p[0].Confirmations != 0 {
		t.Fatalf("pending reorg should reset to pending, got %+v", p[0])
	}
	select {
	case rv := <-rch:
		t.Fatalf("pending reorg must NOT push rollback, got %+v", rv)
	case <-time.After(200 * time.Millisecond):
		// 期望：无事件
	}

	// 推进到最终确认
	g.Tick()
	g.Tick()
	if p := g.Pending(); p[0].Status != DepositCredited {
		t.Fatalf("want credited, got %+v", p[0])
	}

	// 已达最终确认后被重组：触发回滚
	if _, err := g.Reorg(ev.TxHash); err != nil {
		t.Fatalf("credited reorg failed: %v", err)
	}
	if p := g.Pending(); p[0].Status != DepositOrphaned {
		t.Fatalf("want orphaned, got %+v", p[0])
	}
	select {
	case rv := <-rch:
		if rv.Status != DepositOrphaned || rv.TxHash != ev.TxHash {
			t.Fatalf("rollback event wrong: %+v", rv)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watch did not receive rollback event")
	}
}

// 充值深度重组：回退最近 depth 个区块内所有已最终确认的交易，更早确认的交易不受影响。
func TestDepositReorgDepth(t *testing.T) {
	g := NewMockChainGateway(1, time.Hour) // required=1，单 tick 即 Credited
	g.Start()
	defer g.Stop()
	find := func(evs []DepositEvent, tx string) DepositEvent {
		for _, x := range evs {
			if x.TxHash == tx {
				return x
			}
		}
		return DepositEvent{Status: DepositPending}
	}

	e1, _ := g.SubmitDeposit(7301, "USDT", ChainETH, 1000, "")
	g.Tick() // height=1, e1 Credited (BlockHeight=1)
	e2, _ := g.SubmitDeposit(7302, "USDT", ChainETH, 2000, "")
	g.Tick() // height=2, e2 Credited (BlockHeight=2)

	rolled := g.ReorgDepth(1) // cutoff=2，仅回退 BlockHeight>=2 的 e2
	if len(rolled) != 1 || rolled[0].TxHash != e2.TxHash {
		t.Fatalf("depth reorg should roll only newest block, got %+v", rolled)
	}
	h := g.Pending()
	if find(h, e1.TxHash).Status != DepositCredited {
		t.Fatalf("e1 (older) should remain credited, got %+v", h)
	}
	if find(h, e2.TxHash).Status != DepositOrphaned {
		t.Fatalf("e2 (newest) should be orphaned, got %+v", h)
	}

	// 深度=2：回退最近 2 区块（含更早的 e1）
	rolled2 := g.ReorgDepth(2)
	if len(rolled2) != 1 || rolled2[0].TxHash != e1.TxHash {
		t.Fatalf("depth=2 should roll remaining e1, got %+v", rolled2)
	}
}

// 提现重组窗口：已广播但未达安全确认（Broadcasting）的提现被重组应安全回退到 Pending
// （重置确认数、不推送回滚）；未广播（Pending）拒绝；已达最终确认（Credited）才触发回滚。
func TestWithdrawReorgWindow(t *testing.T) {
	g := NewMockWithdrawGateway(2, time.Hour)
	g.Start()
	defer g.Stop()

	ev, _ := g.SubmitWithdraw(8101, "USDT", ChainETH, 500, 1, "0xaddr", false)

	// 未广播即重组：拒绝
	if _, err := g.WithdrawReorg(ev.TxHash); err == nil {
		t.Fatalf("pending withdraw reorg should be rejected")
	}

	g.Tick() // Pending -> Broadcasting (confirmations=1)
	if p := g.WithdrawHistory(); p[0].Status != WithdrawBroadcasting {
		t.Fatalf("want broadcasting, got %+v", p[0])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrh, _ := g.WatchWithdrawRollback(ctx)

	// 已广播未达安全确认即重组：安全回退到 Pending，不推送回滚
	got, err := g.WithdrawReorg(ev.TxHash)
	if err != nil {
		t.Fatalf("broadcasting reorg should succeed: %v", err)
	}
	if got.Status != WithdrawPending || got.Confirmations != 0 {
		t.Fatalf("broadcasting reorg should reset to pending, got %+v", got)
	}
	select {
	case rv := <-wrh:
		t.Fatalf("broadcasting reorg must NOT push rollback, got %+v", rv)
	case <-time.After(200 * time.Millisecond):
	}

	// 重新推进到最终确认（Pending->Broadcasting->Credited 需两次 Tick）
	g.Tick() // Pending -> Broadcasting (confirmations=1)
	g.Tick() // Broadcasting -> Credited (confirmations=2)
	if p := g.WithdrawHistory(); p[0].Status != WithdrawCredited {
		t.Fatalf("want credited, got %+v", p[0])
	}
	got2, err := g.WithdrawReorg(ev.TxHash)
	if err != nil {
		t.Fatalf("credited reorg failed: %v", err)
	}
	if got2.Status != WithdrawOrphaned {
		t.Fatalf("want orphaned, got %+v", got2)
	}
	select {
	case rv := <-wrh:
		if rv.Status != WithdrawOrphaned || rv.TxHash != ev.TxHash {
			t.Fatalf("rollback event wrong: %+v", rv)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watch did not receive rollback event")
	}
}

// 提现深度重组：回退最近 depth 个区块内所有已最终确认的提现，更早确认的提现不受影响。
func TestWithdrawReorgDepth(t *testing.T) {
	g := NewMockWithdrawGateway(1, time.Hour) // required=1：Pending→Broadcasting→Credited 需两次 Tick
	g.Start()
	defer g.Stop()
	find := func(txs []WithdrawEvent, tx string) WithdrawEvent {
		for _, x := range txs {
			if x.TxHash == tx {
				return x
			}
		}
		return WithdrawEvent{Status: WithdrawPending}
	}

	e1, _ := g.SubmitWithdraw(8201, "USDT", ChainETH, 100, 0, "0xa1", false)
	g.Tick()
	g.Tick() // e1 Credited (height=2, BlockHeight=2)
	e2, _ := g.SubmitWithdraw(8202, "USDT", ChainETH, 200, 0, "0xa2", false)
	g.Tick()
	g.Tick() // e2 Credited (height=4, BlockHeight=4)

	rolled := g.WithdrawReorgDepth(1) // cutoff=4，仅回退 BlockHeight>=4 的 e2
	if len(rolled) != 1 || rolled[0].TxHash != e2.TxHash {
		t.Fatalf("depth reorg should roll only newest block, got %+v", rolled)
	}
	h := g.WithdrawHistory()
	if find(h, e1.TxHash).Status != WithdrawCredited {
		t.Fatalf("e1 should remain credited, got %+v", h)
	}
	if find(h, e2.TxHash).Status != WithdrawOrphaned {
		t.Fatalf("e2 should be orphaned, got %+v", h)
	}

	rolled3 := g.WithdrawReorgDepth(2) // cutoff=2，回退剩余 e1
	if len(rolled3) != 1 || rolled3[0].TxHash != e1.TxHash {
		t.Fatalf("depth=2 should roll remaining e1, got %+v", rolled3)
	}
}

// TestFeeModel 验证多链/多资产手续费模型的登记、查询与估算。
func TestFeeModel(t *testing.T) {
	m := NewFeeModel()
	if _, ok := m.Lookup(ChainETH, "USDT"); ok {
		t.Fatal("ETH-USDT should be unregistered initially")
	}
	if got := m.Estimate(ChainETH, "USDT", 1000); got != 0 {
		t.Fatalf("unregistered fee should be 0, got %.4f", got)
	}

	// 基础费 0.1，费率 0.001 -> 1000 提现：0.1 + 1 = 1.1
	m.Register(ChainETH, "USDT", 0.1, 0.001)
	f, ok := m.Lookup(ChainETH, "USDT")
	if !ok || f.Base != 0.1 || f.Rate != 0.001 {
		t.Fatalf("lookup mismatch: %+v ok=%v", f, ok)
	}
	if !approx(m.Estimate(ChainETH, "USDT", 1000), 1.1, 1e-9) {
		t.Fatalf("expect 1.1, got %.6f", m.Estimate(ChainETH, "USDT", 1000))
	}
	if !approx(m.Estimate(ChainETH, "USDT", 0), 0.1, 1e-12) {
		t.Fatalf("amount 0 should be base only 0.1, got %.6f", m.Estimate(ChainETH, "USDT", 0))
	}

	// 纯基础费（BTC）：固定 0.0005
	m.Register(ChainBTC, "BTC", 0.0005, 0)
	if !approx(m.Estimate(ChainBTC, "BTC", 5000), 0.0005, 1e-12) {
		t.Fatalf("expect 0.0005, got %.8f", m.Estimate(ChainBTC, "BTC", 5000))
	}
}
