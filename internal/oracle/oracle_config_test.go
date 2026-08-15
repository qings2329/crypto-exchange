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
func TestNewFromConfigStatic(t *testing.T) {
	conf := OracleConf{
		Feeds: map[string][]FeedSpec{
			"BTC_USDT": {
				{Name: "binance", Type: "static", Price: 50000},
				{Name: "okx", Type: "static", Price: 50040},
			},
		},
	}
	o := NewFromConfig(conf)
	o.Start()
	defer o.Stop()
	time.Sleep(20 * time.Millisecond)
	price, ok := o.IndexPrice("BTC_USDT")
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
