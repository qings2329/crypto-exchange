package es

import (
	"context"
	"testing"
)

// TestNewEmptyURLReturnsMem 验证 url 为空时返回内存实现（不依赖外部 ES）。
func TestNewEmptyURLReturnsMem(t *testing.T) {
	idx := New("", "")
	if idx == nil {
		t.Fatal("New returned nil for empty url")
	}
	if err := idx.Index(context.Background(), TradeDoc{Symbol: "BTCUSDT", Price: 1, Qty: 2, Ts: 100}); err != nil {
		t.Fatalf("mem index: %v", err)
	}
	got, err := idx.Search(context.Background(), TradeQuery{Symbol: "BTCUSDT", Limit: 10})
	if err != nil || len(got) != 1 {
		t.Fatalf("mem search: err=%v len=%d", err, len(got))
	}
}

// TestMemIndexerRoundTrip 验证内存实现的索引/检索：过滤、时间窗、降序、limit、幂等。
func TestMemIndexerRoundTrip(t *testing.T) {
	idx := newMemIndexer()
	ctx := context.Background()
	trades := []TradeDoc{
		{Symbol: "BTCUSDT", Price: 100, Qty: 1, TakerSide: "buy", Ts: 1000},
		{Symbol: "BTCUSDT", Price: 110, Qty: 2, TakerSide: "sell", Ts: 2000},
		{Symbol: "ETHUSDT", Price: 10, Qty: 5, TakerSide: "buy", Ts: 1500},
		{Symbol: "BTCUSDT", Price: 120, Qty: 3, TakerSide: "buy", Ts: 3000},
	}
	for _, d := range trades {
		if err := idx.Index(ctx, d); err != nil {
			t.Fatalf("index: %v", err)
		}
	}

	// 按 symbol 过滤 + 时间窗 [0, 2500]。
	got, err := idx.Search(ctx, TradeQuery{Symbol: "BTCUSDT", From: 0, To: 2500, Limit: 100})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 BTCUSDT in window, got %d: %+v", len(got), got)
	}
	// 降序：Ts 2000 在 1000 之前。
	if got[0].Ts != 2000 || got[1].Ts != 1000 {
		t.Fatalf("desc order wrong: %+v", got)
	}

	// 按 side 过滤。
	buy, _ := idx.Search(ctx, TradeQuery{Symbol: "BTCUSDT", Side: "buy", Limit: 100})
	if len(buy) != 2 {
		t.Fatalf("expected 2 BTCUSDT buy, got %d", len(buy))
	}

	// limit 截断到最新 N 条。
	lim, _ := idx.Search(ctx, TradeQuery{Symbol: "BTCUSDT", Limit: 1})
	if len(lim) != 1 || lim[0].Ts != 3000 {
		t.Fatalf("limit wrong: %+v", lim)
	}

	// 幂等：同字段重复 Index 仅存一份。
	if err := idx.Index(ctx, trades[0]); err != nil {
		t.Fatalf("re-index: %v", err)
	}
	all, _ := idx.Search(ctx, TradeQuery{Symbol: "BTCUSDT", Limit: 100})
	if len(all) != 3 {
		t.Fatalf("expected idempotent 3 BTCUSDT, got %d", len(all))
	}
}
