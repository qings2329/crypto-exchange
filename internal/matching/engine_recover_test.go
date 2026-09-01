package matching_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/persist"
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
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

// waitTrades 轮询直到引擎成交流水达到 want 笔（仅测试辅助）。
func waitTrades(t *testing.T, e *matching.Engine, want int) bool {
	t.Helper()
	for i := 0; i < 100; i++ {
		if len(e.ListTrades(0, "", 0)) >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestSubmitWritesWAL(t *testing.T) {
	store := persist.NewMemStore()
	e := matching.NewEngine(nil, nil)
	e.UseStore(store, "n1", 0)
	e.Register("BTC_USDT")

	o := &matching.Order{Side: matching.Buy, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8), Time: 1}
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
	buy := &matching.Order{Side: matching.Buy, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8), Time: 1}
	sell := &matching.Order{Side: matching.Sell, Price: matching.FixedFromFloat(200, 2), Qty: matching.FixedFromFloat(1, 8), Time: 2}
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
	if !approx(bids[0].Price.Float(), 100, 1e-9) || !approx(bids[0].Orders[0].Qty.Float(), 1, 1e-9) {
		t.Fatalf("recovered bid wrong: %+v", bids[0])
	}
	if !approx(asks[0].Price.Float(), 200, 1e-9) || !approx(asks[0].Orders[0].Qty.Float(), 1, 1e-9) {
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

	buy := &matching.Order{Side: matching.Buy, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8), Time: 1}
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

// TestEngineRecoverPersistsTradesAndOrders：成交流水与已离场订单的历史经持久层「重启」后
// 仍可被恢复（项2：消除原「成交流水仅内存、重启即丢」的缺口）。引擎 A 撮合出成交并持久化，
// 引擎 B 用同一 Store 恢复，应能看到这笔成交与两笔 filled 订单。
func TestEngineRecoverPersistsTradesAndOrders(t *testing.T) {
	store := persist.NewMemStore()
	ctx := context.Background()

	e1 := matching.NewEngine(nil, nil)
	e1.UseStore(store, "n1", 0)
	e1.Register("BTC_USDT")

	// 互撮：买/卖同价同量 → 1 笔成交，双方 filled 离场。
	buy := &matching.Order{UserID: 1, Side: matching.Buy, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8), Time: 1}
	sell := &matching.Order{UserID: 2, Side: matching.Sell, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8), Time: 2}
	if !e1.Submit("BTC_USDT", buy) || !e1.Submit("BTC_USDT", sell) {
		t.Fatal("submit failed")
	}
	// 等待撮合完成（成交流水出现表示异步 run goroutine 已处理并 applyTrades）。
	if !waitTrades(t, e1, 1) {
		t.Fatalf("engine1 should have 1 trade")
	}

	// 引擎 A「崩溃」，引擎 B 用同一 Store 恢复历史。
	e2 := matching.NewEngine(nil, nil)
	e2.UseStore(store, "n2", 0)
	if err := e2.Recover(ctx); err != nil {
		t.Fatal(err)
	}

	// 成交流水恢复。
	trades := e2.ListTrades(0, "", 0)
	if len(trades) != 1 {
		t.Fatalf("recovered trades should be 1, got %d", len(trades))
	}
	if !approx(trades[0].Price.Float(), 100, 1e-9) {
		t.Fatalf("recovered trade price wrong: %v", trades[0].Price)
	}
	// 按用户维度查询也应命中（userTrades 已恢复）。
	if ut := e2.ListTrades(1, "", 0); len(ut) != 1 {
		t.Fatalf("user 1 trades should be 1, got %d", len(ut))
	}

	// 已离场订单历史恢复（两笔 filled）。
	orders := e2.ListOrders(0, "", "", 0)
	if len(orders) != 2 {
		t.Fatalf("recovered orders should be 2, got %d", len(orders))
	}
	for _, o := range orders {
		if string(o.Status) != "filled" {
			t.Fatalf("recovered order status should be filled, got %s (id=%d)", o.Status, o.ID)
		}
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

// TestEngineRecoverPersistsTradesAndOrdersMySQL 在真实 MySQLStore 上验证项2：
// 引擎 A 撮合出成交并持久化到 ce_matching_trades / ce_matching_orders → 引擎 B 用同一 Store
// 从 MySQL「重启」恢复出成交流水与订单登记（不丢历史）。需 MYSQL_TEST_DSN；无则跳过
// （与 §17.1 的 MySQL 端到端同门控）。这补齐了 §17.1 仅验证订单簿/WAL 恢复的缺口。
func TestEngineRecoverPersistsTradesAndOrdersMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL engine recover test")
	}
	store, err := persist.NewMySQLStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.New(store.DB(), persist.Migrations()).Up(); err != nil {
		t.Fatal(err)
	}

	// 引擎 A 撮合出 1 笔成交（双方 filled 离场），落库。
	e1 := matching.NewEngine(nil, nil)
	e1.UseStore(store, "n1", 0)
	e1.Register("BTC_USDT")
	buy := &matching.Order{UserID: 1, Side: matching.Buy, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8), Time: 1}
	sell := &matching.Order{UserID: 2, Side: matching.Sell, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8), Time: 2}
	if !e1.Submit("BTC_USDT", buy) || !e1.Submit("BTC_USDT", sell) {
		t.Fatal("submit failed")
	}
	if !waitTrades(t, e1, 1) {
		t.Fatal("e1 should have 1 trade")
	}

	// 引擎 B 用同一 Store 从 MySQL 恢复（模拟进程崩溃→新节点接管）。
	e2 := matching.NewEngine(nil, nil)
	e2.UseStore(store, "n2", 0)
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if trades := e2.ListTrades(0, "", 0); len(trades) != 1 {
		t.Fatalf("recovered trades want 1 got %d", len(trades))
	}
	if orders := e2.ListOrders(0, "", "", 0); len(orders) != 2 {
		t.Fatalf("recovered orders want 2 got %d", len(orders))
	}
}
