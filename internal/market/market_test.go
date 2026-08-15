package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
)

func TestApplyTrade(t *testing.T) {
	m := NewMarket()
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 100, Qty: 2, TakerSide: "buy", Ts: 1})
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 120, Qty: 3, TakerSide: "sell", Ts: 2})
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 90, Qty: 1, TakerSide: "buy", Ts: 3})

	tk := m.Snapshot("BTCUSDT")
	if tk == nil {
		t.Fatal("ticker missing")
	}
	if tk.Open24h != 100 {
		t.Fatalf("open24h: %v", tk.Open24h)
	}
	if tk.High24h != 120 {
		t.Fatalf("high24h: %v", tk.High24h)
	}
	if tk.Low24h != 90 {
		t.Fatalf("low24h: %v", tk.Low24h)
	}
	if tk.Last != 90 {
		t.Fatalf("last: %v", tk.Last)
	}
	if tk.Volume24h != 6 {
		t.Fatalf("volume24h: %v", tk.Volume24h)
	}
	// 无盘口时 best bid/ask 以成交价 ±0.05% 兜底（非空）。
	if tk.BestBid == 0 || tk.BestAsk == 0 {
		t.Fatalf("best bid/ask fallback should be non-zero: %+v", tk)
	}

	rt := m.RecentTrades("BTCUSDT", 10)
	if len(rt) != 3 {
		t.Fatalf("recent trades count: %d", len(rt))
	}
	if rt[0].Price != 100 || rt[2].Price != 90 {
		t.Fatalf("recent trades order: %+v", rt)
	}
}

func TestApplyTradeCapsRecent(t *testing.T) {
	m := NewMarket()
	for i := 0; i < recentTradesCap+10; i++ {
		m.ApplyTrade(mq.TradeEvent{Symbol: "X", Price: float64(i), Qty: 1, Ts: int64(i)})
	}
	rt := m.RecentTrades("X", recentTradesCap+100)
	if len(rt) != recentTradesCap {
		t.Fatalf("recent trades should be capped at %d, got %d", recentTradesCap, len(rt))
	}
}

func TestApplyDepth(t *testing.T) {
	m := NewMarket()
	m.ApplyDepth("BTCUSDT",
		[]mq.DepthLevel{{Price: 49900, Volume: 1}, {Price: 49800, Volume: 2}, {Price: 49700, Volume: 3}},
		[]mq.DepthLevel{{Price: 50100, Volume: 1}, {Price: 50200, Volume: 2}})

	tk := m.Snapshot("BTCUSDT")
	if tk == nil || tk.BestBid != 49900 {
		t.Fatalf("best bid should be 49900: %+v", tk)
	}
	if tk.BestAsk != 50100 {
		t.Fatalf("best ask should be 50100: %+v", tk)
	}
	d := m.Depth("BTCUSDT")
	if d == nil || len(d.Bids) != 3 || len(d.Asks) != 2 {
		t.Fatalf("depth mismatch: %+v", d)
	}
}

func TestApplyDepthOverridesFallback(t *testing.T) {
	m := NewMarket()
	// 先有一笔成交（设置兜底 best bid/ask），再到达深度应被真实盘口覆盖。
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 50000, Qty: 1, Ts: 1})
	m.ApplyDepth("BTCUSDT",
		[]mq.DepthLevel{{Price: 49900, Volume: 1}},
		[]mq.DepthLevel{{Price: 50100, Volume: 1}})
	tk := m.Snapshot("BTCUSDT")
	if tk.BestBid != 49900 || tk.BestAsk != 50100 {
		t.Fatalf("depth should override fallback best bid/ask: %+v", tk)
	}
}

// TestKafkaDispatchRoundTrip 验证「Kafka 消息 -> 行情聚合」闭环：用 InMemSubscriber.Feed
// 注入 JSON 消息（与 startKafkaSource 相同的 topic 分发逻辑），确认 Market 被正确更新。
// 该分发逻辑与 server.go 的 startKafkaSource handler 完全一致。
func TestKafkaDispatchRoundTrip(t *testing.T) {
	m := NewMarket()
	handler := func(_ context.Context, topic string, data []byte) error {
		switch topic {
		case "exchange.trades":
			var ev mq.TradeEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				return err
			}
			m.ApplyTrade(ev)
		case "market.depth":
			var ev mq.DepthEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				return err
			}
			m.ApplyDepth(ev.Symbol, ev.Bids, ev.Asks)
		}
		return nil
	}
	sub := mq.NewInMemSubscriber(handler)

	tr := mq.TradeEvent{Symbol: "BTCUSDT", Price: 50000, Qty: 2, TakerSide: "buy", Ts: 1}
	if err := sub.Feed("exchange.trades", mustJSON(t, tr)); err != nil {
		t.Fatalf("feed trade: %v", err)
	}
	dp := mq.DepthEvent{Symbol: "BTCUSDT",
		Bids: []mq.DepthLevel{{Price: 49900, Volume: 1}},
		Asks: []mq.DepthLevel{{Price: 50100, Volume: 1}}, Ts: 2}
	if err := sub.Feed("market.depth", mustJSON(t, dp)); err != nil {
		t.Fatalf("feed depth: %v", err)
	}

	tk := m.Snapshot("BTCUSDT")
	if tk == nil || tk.Last != 50000 || tk.Volume24h != 2 || tk.BestBid != 49900 || tk.BestAsk != 50100 {
		t.Fatalf("ticker after dispatch: %+v", tk)
	}
	if rt := m.RecentTrades("BTCUSDT", 10); len(rt) != 1 || rt[0].Price != 50000 {
		t.Fatalf("recent after dispatch: %+v", rt)
	}
	if d := m.Depth("BTCUSDT"); d == nil || len(d.Bids) != 1 {
		t.Fatalf("depth after dispatch: %+v", d)
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestApplyTradeBuildsKlines 验证成交流按时间桶聚合出 OHLCV：同桶更新、跨桶收盘进历史。
func TestApplyTradeBuildsKlines(t *testing.T) {
	m := NewMarket()
	// 1m 桶：60000 与 61000 同属 [60000,66000) 桶。
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 100, Qty: 2, Ts: 60000})
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 120, Qty: 3, Ts: 61000})

	cur := m.CurrentCandle("BTCUSDT", "1m")
	if cur == nil {
		t.Fatal("current 1m candle missing")
	}
	if cur.OpenTime != 60000 || cur.Open != 100 || cur.High != 120 || cur.Low != 100 || cur.Close != 120 || cur.Volume != 5 {
		t.Fatalf("current candle mismatch: %+v", cur)
	}

	// 跨桶：120000 属于 [120000,126000) 桶，旧桶应进入历史。
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 90, Qty: 1, Ts: 120000})

	hist := m.Klines("BTCUSDT", "1m", 100)
	if len(hist) != 2 {
		t.Fatalf("expected 2 candles (1 closed + 1 current), got %d", len(hist))
	}
	if hist[0].OpenTime != 60000 || hist[0].Close != 120 || hist[0].Volume != 5 {
		t.Fatalf("closed candle mismatch: %+v", hist[0])
	}
	if hist[1].OpenTime != 120000 || hist[1].Open != 90 {
		t.Fatalf("current candle mismatch: %+v", hist[1])
	}

	// 各周期都应被聚合（1h 桶起点不同）。
	cur1h := m.CurrentCandle("BTCUSDT", "1h")
	if cur1h == nil || cur1h.Interval != "1h" {
		t.Fatalf("1h candle missing: %+v", cur1h)
	}
}

// TestApplyTradeSplitsKlineVolume 验证 K 线成交量按 taker 买卖方向拆分：
// 同桶内的买/卖成交分别累加到 Buy/Sell Volume 与 QuoteVolume，且两侧之和等于总量。
func TestApplyTradeSplitsKlineVolume(t *testing.T) {
	m := NewMarket()
	// 同属 1m 桶 [60000,66000)：一笔 taker 买 + 一笔 taker 卖。
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 100, Qty: 2, TakerSide: "buy", Ts: 60000})
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 120, Qty: 3, TakerSide: "sell", Ts: 61000})

	c := m.CurrentCandle("BTCUSDT", "1m")
	if c == nil {
		t.Fatal("current 1m candle missing")
	}
	// 总量与报价量。
	if c.Volume != 5 {
		t.Fatalf("volume: %v", c.Volume)
	}
	if c.QuoteVolume != 100*2+120*3 { // 200 + 360 = 560
		t.Fatalf("quote_volume: %v", c.QuoteVolume)
	}
	// 买卖盘拆分。
	if c.BuyVolume != 2 {
		t.Fatalf("buy_volume: %v", c.BuyVolume)
	}
	if c.SellVolume != 3 {
		t.Fatalf("sell_volume: %v", c.SellVolume)
	}
	if c.BuyQuoteVolume != 200 {
		t.Fatalf("buy_quote_volume: %v", c.BuyQuoteVolume)
	}
	if c.SellQuoteVolume != 360 {
		t.Fatalf("sell_quote_volume: %v", c.SellQuoteVolume)
	}
	// 两侧之和恒等于总量 / 总报价量。
	if c.BuyVolume+c.SellVolume != c.Volume {
		t.Fatalf("buy+sell volume != volume: %v+%v != %v", c.BuyVolume, c.SellVolume, c.Volume)
	}
	if c.BuyQuoteVolume+c.SellQuoteVolume != c.QuoteVolume {
		t.Fatalf("buy+sell quote != quote_volume: %v+%v != %v", c.BuyQuoteVolume, c.SellQuoteVolume, c.QuoteVolume)
	}
}

// TestKlinesReturnsHistoryAndCurrent 验证返回升序、含当前未收盘根、limit 截断。
func TestKlinesReturnsHistoryAndCurrent(t *testing.T) {
	m := NewMarket()
	// 1m 桶，每桶一根，构造 5 根历史 + 1 根当前。
	for i := 0; i < 6; i++ {
		ts := int64(60000 * (i + 1)) // 60000,120000,...,360000
		m.ApplyTrade(mq.TradeEvent{Symbol: "ETHUSDT", Price: float64(10 + i), Qty: 1, Ts: ts})
	}
	all := m.Klines("ETHUSDT", "1m", 100)
	if len(all) != 6 {
		t.Fatalf("expected 6 candles, got %d", len(all))
	}
	// 升序校验。
	for i := 1; i < len(all); i++ {
		if all[i].OpenTime < all[i-1].OpenTime {
			t.Fatalf("candles not ascending: %+v", all)
		}
	}
	// limit 截断到末尾 N 根。
	limited := m.Klines("ETHUSDT", "1m", 3)
	if len(limited) != 3 || limited[2].OpenTime != 360000 {
		t.Fatalf("limit truncation wrong: %+v", limited)
	}
	// 空 symbol 返回空切片而非 nil。
	if got := m.Klines("NOPE", "1m", 10); got == nil {
		t.Fatal("Klines should return empty slice, not nil")
	}
}

// TestHandleKlines 验证路由层参数校验与返回（symbol 必填、interval 合法性、正常返回数组）。
func TestHandleKlines(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := NewMarket()
	m.ApplyTrade(mq.TradeEvent{Symbol: "BTCUSDT", Price: 100, Qty: 2, Ts: 60000})
	s := &Server{market: m}

	cases := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{"missing symbol", "?interval=1m", 400},
		{"invalid interval", "?symbol=BTCUSDT&interval=2m", 400},
		{"ok default interval", "?symbol=BTCUSDT", 200},
		{"ok explicit", "?symbol=BTCUSDT&interval=5m", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest("GET", "/api/v1/market/klines"+tc.query, nil)
			s.handleKlines(c)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == 200 {
				var body []map[string]interface{}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal body: %v", err)
				}
				if len(body) == 0 {
					t.Fatalf("expected non-empty klines array")
				}
			}
		})
	}
}
