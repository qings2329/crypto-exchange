package oracle

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewFromConfigStatic：static 类型喂价源直接生效，聚合为均值。
// 使用不在 DefaultDemoPrices 中的交易对，避免 demo 兜底源干扰断言。
func TestNewFromConfigStatic(t *testing.T) {
	conf := OracleConf{
		Feeds: map[string][]FeedSpec{
			"SOL_USDT": {
				{Name: "binance", Type: "static", Price: 50000},
				{Name: "okx", Type: "static", Price: 50040},
			},
		},
	}
	o := NewFromConfig(conf)
	o.Start()
	defer o.Stop()
	time.Sleep(20 * time.Millisecond)
	price, ok := o.IndexPrice("SOL_USDT")
	if !ok {
		t.Fatal("static 配置应返回指数价")
	}
	if math.Abs(price-50020) > 1e-6 {
		t.Fatalf("两源均值应=50020，got %v", price)
	}
}

// TestNewFromConfigHTTP：真实 REST 适配器经 httptest 验证三交易所解析 + 符号覆盖。
func TestNewFromConfigHTTP(t *testing.T) {
	var gotBinanceSymbol string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/v3/ticker/price"):
			gotBinanceSymbol = r.URL.Query().Get("symbol")
			w.Write([]byte(`{"symbol":"BTCUSDT","price":"100.00"}`))
		case strings.Contains(r.URL.Path, "/api/v5/market/ticker"):
			w.Write([]byte(`{"data":[{"last":"101.00"}]}`))
		case strings.Contains(r.URL.Path, "/v2/prices/"):
			w.Write([]byte(`{"data":{"amount":"102.00","currency":"USD"}}`))
		default:
			http.Error(w, "unknown", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conf := OracleConf{
		PollIntervalSec: 1,
		MinFeeds:        2,
		Feeds: map[string][]FeedSpec{
			"BTC_USDT": {
				{Name: "binance", Type: "http", URL: srv.URL + "/api/v3/ticker/price?symbol=%s", Symbol: "BTCUSDT", Parse: "binance"},
				{Name: "okx", Type: "http", URL: srv.URL + "/api/v5/market/ticker?instId=%s", Symbol: "BTC-USDT", Parse: "okx"},
				{Name: "coinbase", Type: "http", URL: srv.URL + "/v2/prices/%s/spot", Symbol: "BTC-USDT", Parse: "coinbase"},
			},
		},
	}
	o := NewFromConfig(conf)
	o.Start()
	defer o.Stop()
	time.Sleep(50 * time.Millisecond)

	// 符号覆盖：内部 BTC_USDT 应被替换为交易所格式 BTCUSDT 发出。
	if gotBinanceSymbol != "BTCUSDT" {
		t.Fatalf("binance 请求 symbol 应被覆盖为 BTCUSDT，got %q", gotBinanceSymbol)
	}
	price, ok := o.IndexPrice("BTC_USDT")
	if !ok {
		t.Fatal("应已聚合出指数价")
	}
	// 100/101/102 均在中位 101 的 2% 容差内，最终中位 = 101。
	if math.Abs(price-101) > 1e-6 {
		t.Fatalf("指数价应≈101，got %v", price)
	}
}

// TestNewFromConfigFallbackToDemo：交易对配置了真实源但全部不可达（或为空）时，
// NewFromConfig 会追加 2 个 demo 静态源作兜底，使聚合仍可产出指数价（MidFeeds=2），
// 避免 IndexPrice 返回 (0,false) 导致依赖方（结算/强平）跳过。不在 demo 表的交易对
// 若无有效源则仍返回无价。
func TestNewFromConfigFallbackToDemo(t *testing.T) {
	// 场景一：BTC_USDT_PERP 在 DefaultDemoPrices 中，但配置里只有 URL 为空的 http 源
	// （视为无效、不被加入 feeds），应回退到 demo 兜底样本，聚合出 50000。
	conf := OracleConf{
		Feeds: map[string][]FeedSpec{
			"BTC_USDT_PERP": {
				{Name: "binance", Type: "http", URL: "", Symbol: "BTCUSDT", Parse: "binance"},
			},
		},
	}
	o := NewFromConfig(conf)
	o.Start()
	defer o.Stop()
	time.Sleep(20 * time.Millisecond)
	price, ok := o.IndexPrice("BTC_USDT_PERP")
	if !ok {
		t.Fatal("demo 兜底应使 BTC_USDT_PERP 仍产出指数价")
	}
	if math.Abs(price-50000) > 1e-6 {
		t.Fatalf("demo 兜底价应=50000，got %v", price)
	}

	// 场景二：不在 demo 表的交易对、且无任何有效源，仍应返回无价（不臆造价格）。
	if _, ok := o.IndexPrice("SOL_USDT"); ok {
		t.Fatal("不在 demo 表且无源的 SOL_USDT 不应臆造指数价")
	}
}

// TestNewFromConfigEmpty：空配置不提供任何指数价（调用方需回退）。
func TestNewFromConfigEmpty(t *testing.T) {
	o := NewFromConfig(OracleConf{})
	if _, ok := o.IndexPrice("BTC_USDT"); ok {
		t.Fatal("空配置应无指数价")
	}
}

// TestCoinbaseParse：Coinbase spot 价格解析与异常。
func TestCoinbaseParse(t *testing.T) {
	p, err := CoinbaseParse([]byte(`{"data":{"amount":"50000.00","currency":"USD"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p != 50000 {
		t.Fatalf("got %v", p)
	}
	if _, err := CoinbaseParse([]byte(`not json`)); err == nil {
		t.Fatal("非法响应应解析失败")
	}
}
