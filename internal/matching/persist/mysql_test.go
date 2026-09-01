package persist

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// closeF 是浮点近似比较（Fixed 经 Float() 还原后可能含微小舍入，仅用于测试断言）。
func closeF(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}

// setupMySQLStore 创建一个带完整迁移的 MySQLStore（仅在 MYSQL_TEST_DSN 可用时运行）。
func setupMySQLStore(t *testing.T) *MySQLStore {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL integration test")
	}
	s, err := NewMySQLStore(dsn)
	if err != nil {
		t.Fatalf("NewMySQLStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	// 运行迁移建表。
	r := migrate.New(s.DB(), Migrations())
	if err := r.Up(); err != nil {
		t.Fatalf("migrate Up: %v", err)
	}
	// 每个测试从干净状态开始，避免共用同一库时用例间互相污染
	//（既有测试均假设空表/干净 leader；此前依赖每次用全新库才能通过）。
	for _, q := range []string{
		"TRUNCATE ce_matching_wal",
		"TRUNCATE ce_matching_trades",
		"TRUNCATE ce_matching_orders",
		"TRUNCATE ce_matching_snapshot",
		"UPDATE ce_matching_seq SET val=0 WHERE id=1",
		"UPDATE ce_matching_leader SET holder='', expires_at='1970-01-01 00:00:00', heartbeat='1970-01-01 00:00:00' WHERE id=1",
	} {
		if _, err := s.DB().ExecContext(context.Background(), q); err != nil {
			t.Fatalf("clean matching tables: %v", err)
		}
	}
	return s
}

func TestMySQLNextOrderIDMonotonic(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	var prev int64
	for i := 0; i < 50; i++ {
		id, err := s.NextOrderID(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if id <= prev {
			t.Fatalf("order id not monotonic: %d <= %d", id, prev)
		}
		prev = id
	}
}

func TestMySQLSetMinOrderID(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	// 获取当前 ID。
	cur, _ := s.NextOrderID(ctx)
	// SetMinOrderID 不应降低。
	if err := s.SetMinOrderID(ctx, cur-1); err != nil {
		t.Fatal(err)
	}
	id1, _ := s.NextOrderID(ctx)
	if id1 <= cur {
		t.Fatalf("expected id > %d after noop SetMinOrderID, got %d", cur, id1)
	}
	// SetMinOrderID 应提升。
	if err := s.SetMinOrderID(ctx, id1+100); err != nil {
		t.Fatal(err)
	}
	id2, _ := s.NextOrderID(ctx)
	if id2 <= id1+100 {
		t.Fatalf("expected id > %d after SetMinOrderID(%d), got %d", id1+100, id1+100, id2)
	}
}

func TestMySQLWALAppendAndReplay(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if err := s.Append(ctx, matching.OrderEvent{
			Symbol: "BTC_USDT", Type: matching.EventSubmit,
			Order: &matching.Order{ID: int64(i), Side: matching.Buy,
				Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8)},
			Ts: time.Now().UnixNano(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Append(ctx, matching.OrderEvent{
		Symbol: "BTC_USDT", Type: matching.EventCancel, OrderID: 1,
		Ts: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}

	all, err := s.Replay(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("want 4 events, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("seq not ascending: %d then %d", all[i-1].Seq, all[i].Seq)
		}
	}
}

func TestMySQLMaxSeqAndPrune(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	// 空 WAL → MaxSeq = 0。
	max, err := s.MaxSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if max != 0 {
		t.Fatalf("empty WAL max seq: want 0, got %d", max)
	}
	// 追加 5 条。
	for i := 0; i < 5; i++ {
		if err := s.Append(ctx, matching.OrderEvent{
			Symbol: "ETH_USDT", Type: matching.EventSubmit,
			Order: &matching.Order{ID: int64(i + 1), Side: matching.Sell,
				Price: matching.FixedFromFloat(2500, 2), Qty: matching.FixedFromFloat(0.5, 8)},
			Ts: time.Now().UnixNano(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	max, _ = s.MaxSeq(ctx)
	if max != 5 {
		t.Fatalf("after 5 appends: want maxSeq=5, got %d", max)
	}
	// 剪枝前 3 条。
	if err := s.PruneWAL(ctx, 3); err != nil {
		t.Fatal(err)
	}
	rest, _ := s.Replay(ctx, 3)
	if len(rest) != 2 {
		t.Fatalf("after prune(3): want 2 remaining, got %d", len(rest))
	}
}

func TestMySQLSnapshotRoundtrip(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	// 无快照 → version=-1。
	ver, state, err := s.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ver != -1 || state != nil {
		t.Fatalf("empty snapshot: ver=%d state=%v", ver, state)
	}
	// 写入快照。
	payload := []byte(`{"book":"snapshot_data"}`)
	if err := s.SaveSnapshot(ctx, 42, payload); err != nil {
		t.Fatal(err)
	}
	ver, state, err = s.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ver != 42 || string(state) != string(payload) {
		t.Fatalf("snapshot mismatch: ver=%d state=%s", ver, state)
	}
	// 覆盖写。
	if err := s.SaveSnapshot(ctx, 100, []byte(`{"updated":true}`)); err != nil {
		t.Fatal(err)
	}
	ver, state, _ = s.LoadSnapshot(ctx)
	if ver != 100 || string(state) != `{"updated":true}` {
		t.Fatalf("overwrite mismatch: ver=%d state=%s", ver, state)
	}
}

func TestMySQLLeaderMutualExclusion(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	ttl := 10 * time.Second

	ok, _ := s.TryAcquireLeader(ctx, "A", ttl)
	if !ok {
		t.Fatal("A should acquire leader")
	}
	ok, _ = s.TryAcquireLeader(ctx, "B", ttl)
	if ok {
		t.Fatal("B must not acquire while A holds")
	}
	ok, _ = s.RenewLeader(ctx, "A", ttl)
	if !ok {
		t.Fatal("A renew should succeed")
	}
	ok, _ = s.TryAcquireLeader(ctx, "B", ttl)
	if ok {
		t.Fatal("B must not acquire while A holds (renewed)")
	}
	if err := s.ReleaseLeader(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	ok, _ = s.TryAcquireLeader(ctx, "B", ttl)
	if !ok {
		t.Fatal("B should acquire after A released")
	}
	ok, _ = s.TryAcquireLeader(ctx, "A", ttl)
	if ok {
		t.Fatal("A must not re-acquire while B holds")
	}
}

func TestMySQLLeaderExpiryTakeover(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	ok, _ := s.TryAcquireLeader(ctx, "A", 1*time.Millisecond)
	if !ok {
		t.Fatal("A should acquire")
	}
	time.Sleep(10 * time.Millisecond)
	ok, _ = s.TryAcquireLeader(ctx, "B", 10*time.Second)
	if !ok {
		t.Fatal("B should take over after A's lease expired")
	}
	isLeader, _ := s.IsLeader(ctx, "B")
	if !isLeader {
		t.Fatal("B should be leader")
	}
}

func TestMySQLStoreClose(t *testing.T) {
	s := setupMySQLStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMySQLTradeRoundtrip 验证成交流水经 ce_matching_trades 持久化后往返一致（项2 的 MySQL 落库路径）。
// 这是 §17.1 原有 MySQL 端到端验证未覆盖的部分：重启后成交流水从历史库重建。
func TestMySQLTradeRoundtrip(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	pt := matching.PersistedTrade{
		Seq:       1,
		Symbol:    "BTC_USDT",
		Market:    "spot",
		IsMargin:  false,
		Leverage:  0,
		Price:     matching.FixedFromFloat(100, 2),
		Qty:       matching.FixedFromFloat(1, 8),
		TakerID:   1,
		MakerID:   2,
		TakerSide: matching.Buy,
		TakerOID:  10,
		MakerOID:  20,
		Time:      time.Now().UnixNano(),
	}
	if err := s.AppendTrade(ctx, pt); err != nil {
		t.Fatalf("AppendTrade: %v", err)
	}
	pts, err := s.LoadTrades(ctx)
	if err != nil {
		t.Fatalf("LoadTrades: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("want 1 trade, got %d", len(pts))
	}
	got := pts[0]
	if got.Symbol != pt.Symbol || got.Market != pt.Market || got.IsMargin != pt.IsMargin {
		t.Fatalf("trade meta mismatch: %+v", got)
	}
	if got.TakerID != pt.TakerID || got.MakerID != pt.MakerID || got.TakerSide != pt.TakerSide {
		t.Fatalf("trade party mismatch: %+v", got)
	}
	if !closeF(got.Price.Float(), 100) || !closeF(got.Qty.Float(), 1) {
		t.Fatalf("trade amount mismatch: price=%v qty=%v", got.Price, got.Qty)
	}
}

// TestMySQLOrderRoundtrip 验证订单登记经 ce_matching_orders 持久化且同 order_id 幂等覆盖（项2 的 MySQL 落库路径）。
func TestMySQLOrderRoundtrip(t *testing.T) {
	s := setupMySQLStore(t)
	ctx := context.Background()
	po := matching.PersistedOrder{
		ID:          42,
		UserID:      7,
		Symbol:      "BTC_USDT",
		Market:      "futures",
		IsMargin:    true,
		Leverage:    10,
		Side:        matching.Buy,
		Price:       matching.FixedFromFloat(100, 2),
		Qty:         matching.FixedFromFloat(1, 8),
		FilledQty:   matching.FixedFromFloat(0.5, 8),
		TimeInForce: "GTC",
		Status:      matching.OrderStatus("filled"),
		CreatedAt:   1,
		UpdatedAt:   2,
	}
	if err := s.UpsertOrder(ctx, po); err != nil {
		t.Fatalf("UpsertOrder: %v", err)
	}
	// 同 order_id 覆盖：状态改为 canceled、成交量补满（幂等）。
	po.Status = matching.OrderStatus("canceled")
	po.FilledQty = matching.FixedFromFloat(1, 8)
	if err := s.UpsertOrder(ctx, po); err != nil {
		t.Fatalf("UpsertOrder overwrite: %v", err)
	}
	pos, err := s.LoadOrders(ctx)
	if err != nil {
		t.Fatalf("LoadOrders: %v", err)
	}
	if len(pos) != 1 {
		t.Fatalf("upsert should be idempotent (1 row), got %d", len(pos))
	}
	got := pos[0]
	if got.ID != po.ID || got.UserID != po.UserID || got.Symbol != po.Symbol {
		t.Fatalf("order meta mismatch: %+v", got)
	}
	if got.Status != matching.OrderStatus("canceled") {
		t.Fatalf("upsert should overwrite status, got %v", got.Status)
	}
	if !closeF(got.FilledQty.Float(), 1) {
		t.Fatalf("filled qty mismatch: %v", got.FilledQty)
	}
}
