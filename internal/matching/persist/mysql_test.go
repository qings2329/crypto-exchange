package persist

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

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
