package settlement

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// TestEmitDeliversCreditedEvent 回归：充值达安全确认后，credited 事件必须经由订阅 channel
// 送达消费者（驱动内部账本入账）。验证 emit 投递路径在改为「阻塞+超时」后仍正常工作，
// 不破坏既有入账闭环。
func TestEmitDeliversCreditedEvent(t *testing.T) {
	g := NewMockChainGateway(1, time.Hour) // required=1，单次 tick 即入账
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := g.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if _, err := g.SubmitDeposit(1, "USDT", ChainETH, amt(ChainETH, 10), "addr"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	g.Tick() // 达到 required -> credited -> emit
	select {
	case ev := <-ch:
		if ev.Status != DepositCredited {
			t.Fatalf("expected credited, got %s", ev.Status)
		}
		if ev.UserID != 1 || !amtEq(ev.Amount, 10) {
			t.Fatalf("event 字段错配: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("credited 事件未送达订阅者（入账闭环断裂）")
	}
}

// TestEmitBackpressureNotSilent 修复 #4：订阅者背压（channel 满且长期不消费）时，emit 不得静默
// 丢弃 credited 事件，而应阻塞到超时上限后输出 DROPPED 告警（运维可经 Pending() 对账重放），
// 且网关本身不 panic、已入账状态仍可在 g.pending 中恢复。
func TestEmitBackpressureNotSilent(t *testing.T) {
	old := emitSendTimeout
	emitSendTimeout = 30 * time.Millisecond // 调短超时，避免单测耗时
	defer func() { emitSendTimeout = old }()

	g := NewMockChainGateway(1, time.Hour) // required=1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := g.Watch(ctx) // 64 缓冲，本测试全程不读取 -> 易触发背压
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// 先充满 64 个缓冲（一次性 tick 后全入账并 emit，缓冲恰好吸收，不阻塞）。
	for i := 0; i < 64; i++ {
		if _, err := g.SubmitDeposit(int64(i+1), "USDT", ChainETH, amt(ChainETH, 1), "addr"); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	g.Tick()

	// 再提交 1 笔并 tick：其 emit 因缓冲已满而阻塞，超时后输出 DROPPED 告警。
	if _, err := g.SubmitDeposit(999, "USDT", ChainETH, amt(ChainETH, 1), "addr"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	}()
	g.Tick()

	if !strings.Contains(buf.String(), "DROPPED") {
		t.Fatalf("背压时 emit 应输出 DROPPED 告警而非静默丢弃，日志: %q", buf.String())
	}

	// 已入账状态仍可从 g.pending 恢复（对账重放的基础）。
	pending := g.Pending()
	found := false
	for _, ev := range pending {
		if ev.UserID == 999 && ev.Status == DepositCredited {
			found = true
		}
	}
	if !found {
		t.Fatal("背压后 credited 状态未持久化于 Pending()，无法对账恢复")
	}
	_ = ch
}

// captureLog 临时把标准 logger 输出重定向到 buf，返回还原函数。多个提现日志测试复用。
func captureLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	log.SetOutput(buf)
	log.SetFlags(0)
	return buf, func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	}
}

// TestWithdrawCreditedLogsMilestone 验证提现达最终确认时输出 credited 里程碑日志
// （资金离场审计线索），而非在订阅健康时完全无声。
func TestWithdrawCreditedLogsMilestone(t *testing.T) {
	g := NewMockWithdrawGateway(1, time.Hour) // required=1，单次 tick 即 credited
	if _, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 10), AssetAmount{}, "addr", false); err != nil {
		t.Fatalf("submit: %v", err)
	}
	buf, restore := captureLog(t)
	defer restore()
	g.Tick() // -> broadcasting
	g.Tick() // -> credited（模拟确认 +1/ tick，故需两次）
	if !strings.Contains(buf.String(), "withdraw credited") {
		t.Fatalf("提现达最终确认应输出 credited 里程碑日志，实际: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "withdraw broadcasting") {
		t.Fatalf("提现进入确认追踪应输出 broadcasting 日志，实际: %q", buf.String())
	}
}

// TestWithdrawReorgLogsRollback 验证已确认提现被重组丢弃时网关输出 WARN 回滚日志
// （驱动账本 ReverseWithdraw 回拨的审计线索），而非静默。
func TestWithdrawReorgLogsRollback(t *testing.T) {
	g := NewMockWithdrawGateway(1, time.Hour)
	if _, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 10), AssetAmount{}, "addr", false); err != nil {
		t.Fatalf("submit: %v", err)
	}
	g.Tick() // -> broadcasting
	g.Tick() // -> credited（模拟确认 +1/ tick，故需两次）
	if len(g.WithdrawHistory()) == 0 || g.WithdrawHistory()[0].Status != WithdrawCredited {
		t.Fatal("期望 reorg 前已 credited")
	}
	buf, restore := captureLog(t)
	defer restore()
	if _, err := g.WithdrawReorg(g.WithdrawHistory()[0].TxHash); err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if !strings.Contains(buf.String(), "REORG rollback") {
		t.Fatalf("credited 提现重组应输出 REORG rollback WARN 日志，实际: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "ledger reversal required") {
		t.Fatalf("回滚日志应标注需账本回拨，实际: %q", buf.String())
	}
}

// TestWithdrawReorgDepthLogsRollback 验证深度组逐笔输出回滚 WARN 并附汇总日志。
func TestWithdrawReorgDepthLogsRollback(t *testing.T) {
	g := NewMockWithdrawGateway(2, time.Hour) // required=2
	if _, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 10), AssetAmount{}, "a", false); err != nil {
		t.Fatalf("submit1: %v", err)
	}
	if _, err := g.SubmitWithdraw(2, "ETH", ChainETH, amt(ChainETH, 1), AssetAmount{}, "b", false); err != nil {
		t.Fatalf("submit2: %v", err)
	}
	g.Tick() // height=1: broadcasting (conf 1)
	g.Tick() // height=2: credited (conf 2)
	credited := 0
	for _, ev := range g.WithdrawHistory() {
		if ev.Status == WithdrawCredited {
			credited++
		}
	}
	if credited != 2 {
		t.Fatalf("期望 2 笔 credited，实际 %d", credited)
	}
	buf, restore := captureLog(t)
	defer restore()
	rolled := g.WithdrawReorgDepth(1) // 回退最近 1 区块（height=2 的两笔）
	if len(rolled) != 2 {
		t.Fatalf("期望深度重组回滚 2 笔，实际 %d", len(rolled))
	}
	if !strings.Contains(buf.String(), "REORG rollback") {
		t.Fatalf("深度重组应逐笔输出 REORG rollback 日志，实际: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "depth reorg applied") {
		t.Fatalf("深度重组应输出汇总日志，实际: %q", buf.String())
	}
}
