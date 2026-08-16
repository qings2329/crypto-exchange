package settlement

import (
	"context"
	"testing"
	"time"
)

// TestStartStopStart 验证 Stop 后再次 Start 仍能运行确认循环（#3）：原实现 Stop 关闭 g.stop
// 后未重建，再次 Start 的 goroutine 命中已关闭 channel 立即退出（确认循环失效），且再次 Stop
// 会对已关闭 channel 调 close 触发 panic。修复后 Start 内重建 g.stop。
func TestStartStopStart(t *testing.T) {
	g := NewMockChainGateway(2, 10*time.Millisecond)
	g.Start()
	g.Stop()
	g.Start() // 不应 panic
	defer g.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := g.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if _, err := g.SubmitDeposit(1, "USDT", ChainETH, 1.0, ""); err != nil {
		t.Fatalf("submit: %v", err)
	}
	select {
	case <-ch:
		// 收到 credited 事件，确认循环在第二次 Start 后正常运行。
	case <-time.After(time.Second):
		t.Fatal("第二次 Start 后确认循环未推进到 Credited（疑似失效）")
	}
}
