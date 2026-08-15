package influxdb

import (
	"context"
	"testing"
)

// TestNewEmptyURLReturnsMem 验证 url 为空时返回内存实现（不依赖外部 InfluxDB）。
func TestNewEmptyURLReturnsMem(t *testing.T) {
	s := New("", "", "", "")
	if s == nil {
		t.Fatal("New returned nil for empty url")
	}
	// 内存实现应支持无网络下的写入与回取。
	if err := s.Write(context.Background(), Candle{Symbol: "BTCUSDT", Interval: "1m", OpenTime: 1000, Close: 1}); err != nil {
		t.Fatalf("mem write: %v", err)
	}
	got, err := s.Query(context.Background(), "BTCUSDT", "1m", 0, 0, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("mem query: err=%v len=%d", err, len(got))
	}
}

// TestMemStoreRoundTrip 验证内存实现的写入/回取：升序、时间窗过滤、limit 截断。
func TestMemStoreRoundTrip(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	// 构造 5 根 1m 桶，OpenTime = 60000,120000,...,300000。
	for i := 0; i < 5; i++ {
		c := Candle{
			Symbol:    "BTCUSDT",
			Interval:  "1m",
			OpenTime:  int64(60000 * (i + 1)),
			Close:     float64(i + 1),
			Volume:    float64(i + 1),
			QuoteVolume: float64(i + 1) * 10,
		}
		if err := s.Write(ctx, c); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// 回取全部（end=0 取到末尾）。
	all, err := s.Query(ctx, "BTCUSDT", "1m", 0, 0, 0)
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].OpenTime < all[i-1].OpenTime {
			t.Fatalf("not ascending: %+v", all)
		}
	}
	if all[0].Close != 1 || all[4].Close != 5 {
		t.Fatalf("values mismatch: %+v", all)
	}

	// 时间窗过滤：[0, 180000) 取前 2 根（60000,120000）。
	sub, err := s.Query(ctx, "BTCUSDT", "1m", 0, 180000, 0)
	if err != nil {
		t.Fatalf("query window: %v", err)
	}
	if len(sub) != 2 || sub[0].OpenTime != 60000 || sub[1].OpenTime != 120000 {
		t.Fatalf("window filter wrong: %+v", sub)
	}

	// limit 截断到末尾 N 根。
	limited, err := s.Query(ctx, "BTCUSDT", "1m", 0, 0, 3)
	if err != nil {
		t.Fatalf("query limit: %v", err)
	}
	if len(limited) != 3 || limited[2].OpenTime != 300000 {
		t.Fatalf("limit truncation wrong: %+v", limited)
	}
}

// TestMemStoreUpsertIdempotent 验证同桶覆盖（幂等），不重复追加。
func TestMemStoreUpsertIdempotent(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	base := Candle{Symbol: "ETHUSDT", Interval: "5m", OpenTime: 60000, Open: 1, Close: 2, Volume: 1}
	if err := s.Write(ctx, base); err != nil {
		t.Fatal(err)
	}
	// 同桶收盘后回填（更高 high/low、更大 volume），应覆盖而非新增。
	upd := base
	upd.High = 9
	upd.Low = 0.5
	upd.Volume = 5
	if err := s.Write(ctx, upd); err != nil {
		t.Fatal(err)
	}
	got, err := s.Query(ctx, "ETHUSDT", "5m", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected upsert to keep 1 candle, got %d", len(got))
	}
	if got[0].High != 9 || got[0].Volume != 5 {
		t.Fatalf("upsert values wrong: %+v", got[0])
	}
}
