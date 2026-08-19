package persist

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/matching"
)

func TestMemNextOrderIDMonotonic(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	var prev int64
	for i := 0; i < 100; i++ {
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

func TestMemWALReplayAndPrune(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	// 提交两笔订单，再撤一笔。
	for i := 1; i <= 2; i++ {
		if err := s.Append(ctx, matching.OrderEvent{
			Symbol: "BTC_USDT", Type: matching.EventSubmit,
			Order: &matching.Order{ID: int64(i), Side: matching.Buy, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8)},
			Ts:    time.Now().UnixNano(),
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

	// Replay(0) 应返回全部 3 条，且 seq 升序。
	all, err := s.Replay(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 events, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("events not ascending by seq: %d then %d", all[i-1].Seq, all[i].Seq)
		}
	}

	// 快照覆盖到 maxSeq，并剪枝。
	maxSeq, _ := s.MaxSeq(ctx)
	if err := s.SaveSnapshot(ctx, maxSeq, []byte(`{"snapshot":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneWAL(ctx, maxSeq); err != nil {
		t.Fatal(err)
	}
	// 剪枝后 Replay(maxSeq) 应为空。
	rest, _ := s.Replay(ctx, maxSeq)
	if len(rest) != 0 {
		t.Fatalf("wal not pruned: %d remaining", len(rest))
	}
	// 快照可读。
	ver, state, err := s.LoadSnapshot(ctx)
	if err != nil || ver != maxSeq {
		t.Fatalf("snapshot load mismatch: ver=%d want %d err=%v", ver, maxSeq, err)
	}
	if string(state) != `{"snapshot":true}` {
		t.Fatalf("snapshot state mismatch: %s", state)
	}
}

func TestMemLeaderMutualExclusion(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	ttl := 10 * time.Second

	// A 获得 leader。
	ok, _ := s.TryAcquireLeader(ctx, "A", ttl)
	if !ok {
		t.Fatal("A should acquire leader")
	}
	// B 在租约内不能获得。
	ok, _ = s.TryAcquireLeader(ctx, "B", ttl)
	if ok {
		t.Fatal("B must not acquire while A holds lease")
	}
	// A 续约成功。
	ok, _ = s.RenewLeader(ctx, "A", ttl)
	if !ok {
		t.Fatal("A renew should succeed")
	}
	// B 仍不能。
	ok, _ = s.TryAcquireLeader(ctx, "B", ttl)
	if ok {
		t.Fatal("B must not acquire while A holds (renewed) lease")
	}
	// A 释放后 B 可获得。
	if err := s.ReleaseLeader(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	ok, _ = s.TryAcquireLeader(ctx, "B", ttl)
	if !ok {
		t.Fatal("B should acquire after A released")
	}
	// A 不能再获得（B 持有）。
	ok, _ = s.TryAcquireLeader(ctx, "A", ttl)
	if ok {
		t.Fatal("A must not re-acquire while B holds")
	}
}

func TestMemLeaderExpiryTakeover(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	// 极短租约：A 获得后过期，B 应可接管。
	ok, _ := s.TryAcquireLeader(ctx, "A", 1*time.Millisecond)
	if !ok {
		t.Fatal("A should acquire")
	}
	time.Sleep(5 * time.Millisecond)
	ok, _ = s.TryAcquireLeader(ctx, "B", 10*time.Second)
	if !ok {
		t.Fatal("B should take over after A's lease expired")
	}
}

// 确保 OrderEvent 可正常 JSON 往返（MySQLStore 依赖此）。
func TestOrderEventJSONRoundTrip(t *testing.T) {
	ev := matching.OrderEvent{
		Seq: 7, Symbol: "ETH_USDT", Type: matching.EventSubmit,
		Order: &matching.Order{ID: 7, UserID: 3, Side: matching.Sell, Price: matching.FixedFromFloat(250, 2), Qty: matching.FixedFromFloat(2, 8)},
		Ts:    123456,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var got matching.OrderEvent
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Seq != 7 || got.Symbol != "ETH_USDT" || got.Order == nil || got.Order.Qty.Float() != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
