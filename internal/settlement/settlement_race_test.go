package settlement

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestWatchConcurrentEmitCancel 并发回归：多个 goroutine 经 Watch 订阅充值事件并随时取消，
// 同时并发 SubmitDeposit + 手动 Tick 触发 emit（emit 在锁外向订阅 channel 发送）。
//
// 修复前 Watch 清理 goroutine 会在 ctx 取消时 close(ch)，与 emit「快照 subs → 锁外发送」
// 形成竞态，触发 panic: send on closed channel，直接炸掉后台确认循环。
// 现 Watch 不再关闭 channel（消费者经 ctx.Done 退出），-race 下应无数据竞争/panic。
func TestWatchConcurrentEmitCancel(t *testing.T) {
	g := NewMockChainGateway(1, time.Hour) // required=1，不自动 tick

	var consumers, tickers sync.WaitGroup
	for i := 0; i < 16; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := g.Watch(ctx)
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			for {
				select {
				case <-ch:
				case <-ctx.Done():
					return
				}
			}
		}()
		go func(d time.Duration) {
			time.Sleep(d)
			cancel()
		}(time.Duration(i%5) * time.Millisecond)
	}

	// 并发提交入账并手动 tick 触发 emit，与 Watch 取消竞争。
	for i := 0; i < 200; i++ {
		uid := int64(i + 1)
		if _, err := g.SubmitDeposit(uid, "USDT", ChainETH, 1.0, "0xw"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		tickers.Add(1)
		go func() {
			defer tickers.Done()
			g.Tick()
		}()
	}

	tickers.Wait()
	consumers.Wait()
}
