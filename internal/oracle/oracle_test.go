package oracle

import (
	"context"
	"math"
	"testing"
)

func approx(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// 中位聚合：三源 49900/50000/50100 -> 中位 50000。
func TestAggregateMedian(t *testing.T) {
	v, ok := aggregate([]float64{49900, 50000, 50100}, DefaultOutlierTolerance, DefaultMinFeeds)
	if !ok || !approx(v, 50000, 1e-6) {
		t.Fatalf("median aggregate want 50000, got %.2f ok=%v", v, ok)
	}
}

// 离群剔除：Binance 插针报 40000，其余 49900/50000/50100 -> 剔除后中位 50000。
func TestAggregateOutlierRejected(t *testing.T) {
	prices := []float64{40000, 49900, 50000, 50100}
	v, ok := aggregate(prices, DefaultOutlierTolerance, DefaultMinFeeds)
	if !ok || !approx(v, 50000, 1e-6) {
		t.Fatalf("outlier should be rejected, want 50000, got %.2f ok=%v", v, ok)
	}
}

// 双源一致：两源偏差 < 容差 -> 取均值。
func TestAggregateTwoFeeds(t *testing.T) {
	v, ok := aggregate([]float64{49990, 50010}, DefaultOutlierTolerance, DefaultMinFeeds)
	if !ok || !approx(v, 50000, 1e-6) {
		t.Fatalf("two feeds want 50000, got %.2f ok=%v", v, ok)
	}
}

// 双源分歧过大：偏差 > 容差 -> 不更新（返回 false，交由回退）。
func TestAggregateTwoFeedsDiverge(t *testing.T) {
	_, ok := aggregate([]float64{49000, 51000}, DefaultOutlierTolerance, DefaultMinFeeds)
	if ok {
		t.Fatalf("divergent two feeds should fail to aggregate")
	}
}

// 单源：MinFeeds<=1 时采用该源；MinFeeds>1 时失败。
func TestAggregateSingleFeed(t *testing.T) {
	v, ok := aggregate([]float64{50000}, DefaultOutlierTolerance, 1)
	if !ok || !approx(v, 50000, 1e-6) {
		t.Fatalf("single feed with minFeeds=1 should pass, got %.2f ok=%v", v, ok)
	}
	if _, ok2 := aggregate([]float64{50000}, DefaultOutlierTolerance, 2); ok2 {
		t.Fatalf("single feed with minFeeds=2 should fail")
	}
}

// 无报价：返回 false。
func TestAggregateEmpty(t *testing.T) {
	if _, ok := aggregate(nil, DefaultOutlierTolerance, DefaultMinFeeds); ok {
		t.Fatalf("empty should fail")
	}
}

// Oracle 集成：3 个静态源轮询，Snapshot 收敛到中位，且 RawSnapshot 记录样本。
func TestOraclePoll(t *testing.T) {
	or := New(Config{
		PollInterval: 10 * 1e9, // 不会用到（手动 poll）
		Feeds: map[string][]PriceFeed{
			"BTC_USDT_PERP": {
				NewStaticFeed("binance", 50000),
				NewStaticFeed("okx", 50010),
				NewStaticFeed("coinbase", 49990),
			},
		},
	})
	or.Start()
	defer or.Stop()
	// 首次立即聚合
	or.pollAll()

	v, ok := or.IndexPrice("BTC_USDT_PERP")
	if !ok || !approx(v, 50000, 1e-6) {
		t.Fatalf("oracle index want ~50000, got %.2f ok=%v", v, ok)
	}
	raw := or.RawSnapshot()["BTC_USDT_PERP"]
	if len(raw) != 3 {
		t.Fatalf("raw samples want 3, got %d", len(raw))
	}
}

// 无价保留上次：先聚合出值，再喂全失败源，IndexPrice 仍返回上次值。
func TestOracleKeepsLastOnFailure(t *testing.T) {
	good := NewStaticFeed("good", 50000)
	good2 := NewStaticFeed("good2", 50010)
	bad := &StaticFeed{name: "bad", price: -1} // Fetch 必失败（price<=0）
	or := New(Config{
		Feeds: map[string][]PriceFeed{
			"BTC_USDT_PERP": {good, good2}, // 两有效源 -> 可聚合
		},
	})
	or.pollAll()
	if v, ok := or.IndexPrice("BTC_USDT_PERP"); !ok || !approx(v, 50005, 1e-6) {
		t.Fatalf("want ~50005, got %.2f ok=%v", v, ok)
	}
	// 仅坏源：聚合失败，应保留上次值
	or.cfg.Feeds["BTC_USDT_PERP"] = []PriceFeed{bad}
	or.pollAll()
	if v, ok := or.IndexPrice("BTC_USDT_PERP"); !ok || !approx(v, 50005, 1e-6) {
		t.Fatalf("after failure want to keep 50005, got %.2f ok=%v", v, ok)
	}
}

// 自定义喂价源，用于注入异常值 / panic，验证聚合与轮询的健壮性。
type feedFn struct {
	name string
	f    func(ctx context.Context, symbol string) (float64, error)
}

func (ff feedFn) Name() string { return ff.name }
func (ff feedFn) Fetch(ctx context.Context, symbol string) (float64, error) {
	return ff.f(ctx, symbol)
}

// F5：喂价源返回 NaN/Inf 必须被拒绝，不得污染指数价（原 `p<=0` 无法拦截 NaN/Inf）。
func TestOracleRejectsNonFiniteFeed(t *testing.T) {
	o := New(Config{
		MinFeeds: 1,
		Feeds: map[string][]PriceFeed{
			"BTC_USDT": {
				feedFn{name: "nan", f: func(context.Context, string) (float64, error) { return math.NaN(), nil }},
				feedFn{name: "inf", f: func(context.Context, string) (float64, error) { return math.Inf(1), nil }},
				feedFn{name: "ok", f: func(context.Context, string) (float64, error) { return 50000, nil }},
			},
		},
	})
	o.pollAll()
	v, ok := o.IndexPrice("BTC_USDT")
	if !ok || !validFeedPrice(v) {
		t.Fatalf("index must be valid finite price, got %v ok=%v", v, ok)
	}
	if !approx(v, 50000, 1e-6) {
		t.Fatalf("index should aggregate to 50000, got %v", v)
	}
}

// F5：喂价源 ParseFunc panic 不得拖垮轮询 goroutine（panic 被 recover 捕获，其余源仍生效）。
func TestOraclePanicFeedRecovered(t *testing.T) {
	o := New(Config{
		MinFeeds: 1,
		Feeds: map[string][]PriceFeed{
			"BTC_USDT": {
				feedFn{name: "panic", f: func(context.Context, string) (float64, error) { panic("boom") }},
				feedFn{name: "ok", f: func(context.Context, string) (float64, error) { return 50000, nil }},
			},
		},
	})
	// 不应有 panic 传播到调用方（pollSymbol 内部 recover）。
	o.pollAll()
	v, ok := o.IndexPrice("BTC_USDT")
	if !ok || !approx(v, 50000, 1e-6) {
		t.Fatalf("after panic feed, index should still be 50000, got %v ok=%v", v, ok)
	}
}
