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
	if _, err := g.SubmitDeposit(1, "USDT", ChainETH, 10, "addr"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	g.Tick() // 达到 required -> credited -> emit
	select {
	case ev := <-ch:
		if ev.Status != DepositCredited {
			t.Fatalf("expected credited, got %s", ev.Status)
		}
		if ev.UserID != 1 || ev.Amount != 10 {
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
		if _, err := g.SubmitDeposit(int64(i+1), "USDT", ChainETH, 1, "addr"); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	g.Tick()

	// 再提交 1 笔并 tick：其 emit 因缓冲已满而阻塞，超时后输出 DROPPED 告警。
	if _, err := g.SubmitDeposit(999, "USDT", ChainETH, 1, "addr"); err != nil {
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
