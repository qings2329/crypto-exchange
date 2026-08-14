package matching_test

import (
	"context"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/persist"
)

func approx(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// 等待引擎异步把订单撮合进簿（仅测试辅助）。
func waitDepth(t *testing.T, e *matching.Engine, symbol string, wantBids, wantAsks int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		bids, asks, ok := e.Depth(symbol)
		if ok && len(bids) == wantBids && len(asks) == wantAsks {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	bids, asks, _ := e.Depth(symbol)
	t.Fatalf("depth not as expected: bids=%d asks=%d (want %d/%d)", len(bids), len(asks), wantBids, wantAsks)
}

func TestSubmitWritesWAL(t *testing.T) {
	store := persist.NewMemStore()
	e := matching.NewEngine(nil, nil)
	e.UseStore(store, "n1", 0)
	e.Register("BTC_USDT")

	o := &matching.Order{Side: matching.Buy, Price: 100, Qty: 1, Time: 1}
	if !e.Submit("BTC_USDT", o) {
		t.Fatal("submit failed")
	}
	if o.ID == 0 {
		t.Fatal("order id not assigned by store")
	}

	// WAL 应在 Submit 返回后即可见（同步落盘）。
	events, err := store.Replay(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != matching.EventSubmit || events[0].Order == nil || events[0].Order.ID != o.ID {
		t.Fatalf("WAL missing submit event: %+v", events)
	}
}

// 模拟「进程崩溃」：引擎 A 写入 WAL 后退出，引擎 B 用同一 Store 重建并恢复订单簿。
func TestEngineRecoverFromStore(t *testing.T) {
	store := persist.NewMemStore()
	ctx := context.Background()

	e1 := matching.NewEngine(nil, nil)
	e1.UseStore(store, "n1", 0)
	e1.Register("BTC_USDT")

	// 两笔不互撮的挂单：买 100、卖 200。
	buy := &matching.Order{Side: matching.Buy, Price: 100, Qty: 1, Time: 1}
	sell := &matching.Order{Side: matching.Sell, Price: 200, Qty: 1, Time: 2}
	if !e1.Submit("BTC_USDT", buy) || !e1.Submit("BTC_USDT", sell) {
		t.Fatal("submit failed")
	}
	waitDepth(t, e1, "BTC_USDT", 1, 1)
	maxID := buy.ID
	if sell.ID > maxID {
		maxID = sell.ID
	}

	// 引擎 A「崩溃」，引擎 B 用同一 Store 恢复。
	e2 := matching.NewEngine(nil, nil)
	e2.UseStore(store, "n2", 0)
	if err := e2.Recover(ctx); err != nil {
		t.Fatal(err)
	}

	bids, asks, ok := e2.Depth("BTC_USDT")
	if !ok {
		t.Fatal("depth failed after recover")
	}
	if len(bids) != 1 || len(asks) != 1 {
		t.Fatalf("recovered book mismatch: bids=%d asks=%d", len(bids), len(asks))
	}
	if !approx(bids[0].Price, 100, 1e-9) || !approx(bids[0].Orders[0].Qty, 1, 1e-9) {
		t.Fatalf("recovered bid wrong: %+v", bids[0])
	}
	if !approx(asks[0].Price, 200, 1e-9) || !approx(asks[0].Orders[0].Qty, 1, 1e-9) {
		t.Fatalf("recovered ask wrong: %+v", asks[0])
	}

	// 恢复后新订单号必须严格大于历史最大 ID（不重复）。
	next, err := store.NextOrderID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next <= maxID {
		t.Fatalf("order id reused after recover: next=%d maxID=%d", next, maxID)
	}
}

// 撤单应写入 WAL；恢复后该订单不应出现在簿中。
func TestEngineRecoverReflectsCancel(t *testing.T) {
	store := persist.NewMemStore()
	ctx := context.Background()

	e1 := matching.NewEngine(nil, nil)
	e1.UseStore(store, "n1", 0)
	e1.Register("BTC_USDT")

	buy := &matching.Order{Side: matching.Buy, Price: 100, Qty: 1, Time: 1}
	if !e1.Submit("BTC_USDT", buy) {
		t.Fatal("submit failed")
	}
	waitDepth(t, e1, "BTC_USDT", 1, 0)

	if !e1.Cancel("BTC_USDT", buy.ID) {
		t.Fatal("cancel failed")
	}

	e2 := matching.NewEngine(nil, nil)
	e2.UseStore(store, "n2", 0)
	if err := e2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	bids, _, ok := e2.Depth("BTC_USDT")
	if !ok || len(bids) != 0 {
		t.Fatalf("cancel not reflected after recover: bids=%d ok=%v", len(bids), ok)
	}
}

// Recover 幂等：重复调用不应重复应用 WAL。
func TestEngineRecoverIdempotent(t *testing.T) {
	store := persist.NewMemStore()
	ctx := context.Background()
	e := matching.NewEngine(nil, nil)
	e.UseStore(store, "n1", 0)
	if err := e.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Recover(ctx); err != nil { // 第二次应为 no-op（recovered 标记）
		t.Fatal(err)
	}
}
