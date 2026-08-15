// Package market 是行情服务的「领域层 + HTTP Handler 层」。
//
// 分层定位：
//   - 领域对象 Market（内存行情存储）：维护各交易对的实时 ticker（含 24h 统计）、
//     订单簿深度与近期成交，供 WebSocket 推送与 REST 查询。
//   - 应用装配 Server：创建行情存储、WebSocket Hub，并选择数据源
//     （Kafka 消费 或 撮合引擎 WebSocket 行情流，或无源时演示喂价），
//     通过 RegisterRoutes 暴露 /ws、/api/v1/market/ticker、/depth、/trades。
//   - cmd/market/main.go 仅做进程级装配：读配置、建日志、调用 NewServer + RegisterRoutes + Run。
//
// 行情由撮合引擎经 Kafka（exchange.trades / market.depth）驱动；无 Kafka 时由
// cmd/matching 的 WebSocket 行情流驱动（与 spot 同构）。演示随机游走仅作最后兜底。
package market

import (
	"context"
	"sync"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/influxdb"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
)

const (
	recentTradesCap = 100 // 每交易对保留的近期成交条数上限
	depthCap        = 20  // 对外暴露的盘口档位上限
	klineCap        = 500 // 每交易对每周期保留的 K 线根数上限（环形缓冲）
)

// Intervals 是支持的 K 线周期（有序，决定 WS 推送顺序）。
var Intervals = []string{"1m", "5m", "15m", "30m", "1h", "4h", "1d"}

// IntervalMillis 把周期名映射到毫秒长度，用于把成交时间戳对齐到桶起点。
var IntervalMillis = map[string]int64{
	"1m":  60_000,
	"5m":  300_000,
	"15m": 900_000,
	"30m": 1_800_000,
	"1h":  3_600_000,
	"4h":  14_400_000,
	"1d":  86_400_000,
}

// IsValidInterval 判断周期名是否受支持。
func IsValidInterval(s string) bool {
	_, ok := IntervalMillis[s]
	return ok
}

// bucketStart 把 ts 向下对齐到周期桶起点（unix 毫秒）。
func bucketStart(ts, ms int64) int64 {
	return ts - (ts % ms)
}

// Ticker 某交易对的实时行情快照（含 24h 统计）。
type Ticker struct {
	Symbol    string  `json:"symbol"`
	Last      float64 `json:"last"`
	BestBid   float64 `json:"best_bid"`
	BestAsk   float64 `json:"best_ask"`
	Open24h   float64 `json:"open_24h"`
	High24h   float64 `json:"high_24h"`
	Low24h    float64 `json:"low_24h"`
	Volume24h float64 `json:"volume_24h"` // base 成交量（自首个成交起累计）
	Timestamp int64   `json:"timestamp"`
}

// RecentTrade 一笔近期成交（用于 /trades 与 WS 推送）。
type RecentTrade struct {
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	Qty    float64 `json:"qty"`
	Side   string  `json:"side"` // taker side: buy/sell
	Ts     int64   `json:"ts"`
}

// Depth 某交易对的盘口快照（聚合后的深度行）。
type Depth struct {
	Symbol string           `json:"symbol"`
	Bids   []mq.DepthLevel  `json:"bids"`
	Asks   []mq.DepthLevel  `json:"asks"`
	Ts     int64            `json:"ts"`
}

// Candle 一根 K 线（OHLCV），由成交流按时间桶聚合而成。
// 成交量按主动成交方向（taker）拆分为买方/卖方两侧，便于展示主动买卖盘力量。
type Candle struct {
	Symbol   string  `json:"symbol"`
	Interval string  `json:"interval"`
	OpenTime int64   `json:"open_time"` // 桶起点（unix 毫秒）
	Open     float64 `json:"open"`
	High     float64 `json:"high"`
	Low      float64 `json:"low"`
	Close    float64 `json:"close"`
	Volume   float64 `json:"volume"`    // base 成交量（= BuyVolume + SellVolume）
	QuoteVolume float64 `json:"quote_volume"` // 报价量（= price*qty 累加，计价货币）
	BuyVolume    float64 `json:"buy_volume"`    // taker 主动性买盘 base 量
	SellVolume   float64 `json:"sell_volume"`   // taker 主动性卖盘 base 量
	BuyQuoteVolume  float64 `json:"buy_quote_volume"`  // taker 买盘报价量
	SellQuoteVolume float64 `json:"sell_quote_volume"` // taker 卖盘报价量
	Ts       int64   `json:"ts"`     // 最近一次更新时间
}

// Market 维护各交易对的实时行情，供 WebSocket 推送与 REST 查询。
type Market struct {
	mu          sync.RWMutex
	tickers     map[string]*Ticker
	recentTrades map[string][]*RecentTrade
	depths      map[string]*Depth
	klines      map[string]map[string]*Candle    // symbol -> interval -> 当前未收盘 K 线
	klineHistory map[string]map[string][]*Candle // symbol -> interval -> 已收盘历史（环形）
	// Store 是可选的 K 线持久化层（InfluxDB 等）；为 nil 时仅用内存环形缓冲。
	// 由 NewServer 按配置装配；pushHistory 在收盘时异步落盘，Klines 在请求超内存
	// 环形上限时回取更早历史。一旦设置不再变更，避免与后台写入产生数据竞争。
	Store influxdb.CandleStore
}

// NewMarket 创建空的行情存储。
func NewMarket() *Market {
	return &Market{
		tickers:      make(map[string]*Ticker),
		recentTrades: make(map[string][]*RecentTrade),
		depths:       make(map[string]*Depth),
		klines:       make(map[string]map[string]*Candle),
		klineHistory:  make(map[string]map[string][]*Candle),
	}
}

// SetCandleStore 装配可选的 K 线持久化层（InfluxDB 等）。应在启动期、并发写入前调用一次。
func (m *Market) SetCandleStore(s influxdb.CandleStore) {
	m.Store = s
}

// ApplyTrade 用一笔成交更新行情：刷新 last/时间戳、24h 高低/量，记录近期成交，
// 并在尚无盘口时以成交价 ±0.05% 兜底买卖一档。
func (m *Market) ApplyTrade(ev mq.TradeEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickers[ev.Symbol]
	if !ok {
		t = &Ticker{Symbol: ev.Symbol, Open24h: ev.Price}
		m.tickers[ev.Symbol] = t
	}
	t.Last = ev.Price
	t.Timestamp = ev.Ts
	if t.High24h == 0 || ev.Price > t.High24h {
		t.High24h = ev.Price
	}
	if t.Low24h == 0 || ev.Price < t.Low24h {
		t.Low24h = ev.Price
	}
	t.Volume24h += ev.Qty
	if t.BestBid == 0 {
		t.BestBid = ev.Price * 0.9995
	}
	if t.BestAsk == 0 {
		t.BestAsk = ev.Price * 1.0005
	}

	rt := &RecentTrade{Symbol: ev.Symbol, Price: ev.Price, Qty: ev.Qty, Side: ev.TakerSide, Ts: ev.Ts}
	list := m.recentTrades[ev.Symbol]
	list = append(list, rt)
	if len(list) > recentTradesCap {
		list = list[len(list)-recentTradesCap:]
	}
	m.recentTrades[ev.Symbol] = list

	// K 线聚合：按各周期把成交归入时间桶，更新 OHLCV 及买卖盘拆分。
	if ev.Ts > 0 {
		m.aggregateKlines(ev.Symbol, ev.Price, ev.Qty, ev.Ts, ev.TakerSide)
	}
}

// aggregateKlines 把一笔成交写入每个周期的当前 K 线（调用方须持写锁）。
// takerSide 为 "buy"/"sell"，用于把成交量按主动成交方向拆分为买卖两侧。
func (m *Market) aggregateKlines(symbol string, price, qty float64, ts int64, takerSide string) {
	symKlines, ok := m.klines[symbol]
	if !ok {
		symKlines = make(map[string]*Candle)
		m.klines[symbol] = symKlines
	}
	quote := price * qty
	for interval, ms := range IntervalMillis {
		start := bucketStart(ts, ms)
		cur := symKlines[interval]
		if cur == nil || cur.OpenTime != start {
			// 收盘上一根（若存在）并开新桶。乱序成交（ts 早于当前桶起点）跳过，避免回写已收盘桶。
			if cur != nil && ts >= cur.OpenTime {
				m.pushHistory(symbol, interval, cur)
			}
			c := &Candle{
				Symbol: symbol, Interval: interval, OpenTime: start,
				Open: price, High: price, Low: price, Close: price,
				Volume: qty, QuoteVolume: quote, Ts: ts,
			}
			if takerSide == "buy" {
				c.BuyVolume = qty
				c.BuyQuoteVolume = quote
			} else {
				c.SellVolume = qty
				c.SellQuoteVolume = quote
			}
			symKlines[interval] = c
		} else {
			if price > cur.High {
				cur.High = price
			}
			if price < cur.Low {
				cur.Low = price
			}
			cur.Close = price
			cur.Volume += qty
			cur.QuoteVolume += quote
			cur.Ts = ts
			if takerSide == "buy" {
				cur.BuyVolume += qty
				cur.BuyQuoteVolume += quote
			} else {
				cur.SellVolume += qty
				cur.SellQuoteVolume += quote
			}
		}
	}
}

// pushHistory 把一根已收盘 K 线追加到历史环形缓冲（调用方须持写锁）。
// 若装配了持久化层 Store，则异步把该已收盘 K 线落盘（best-effort，失败仅记录不阻断）。
func (m *Market) pushHistory(symbol, interval string, c *Candle) {
	hist, ok := m.klineHistory[symbol]
	if !ok {
		hist = make(map[string][]*Candle)
		m.klineHistory[symbol] = hist
	}
	list := hist[interval]
	list = append(list, c)
	if len(list) > klineCap {
		list = list[len(list)-klineCap:]
	}
	hist[interval] = list

	if m.Store != nil {
		saved := *c // 拷贝后交后台写入，避免与后续只读访问共享指针。
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = m.Store.Write(ctx, toInfluxCandle(&saved))
		}()
	}
}

// ApplyDepth 用订单簿深度快照更新盘口，并以最优买卖档刷新 ticker 的 best_bid/best_ask。
func (m *Market) ApplyDepth(symbol string, bids, asks []mq.DepthLevel) {
	bids = capLevels(bids, depthCap)
	asks = capLevels(asks, depthCap)
	now := time.Now().UnixMilli()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.depths[symbol] = &Depth{Symbol: symbol, Bids: bids, Asks: asks, Ts: now}

	t, ok := m.tickers[symbol]
	if !ok {
		t = &Ticker{Symbol: symbol}
		m.tickers[symbol] = t
	}
	if len(bids) > 0 {
		t.BestBid = bids[0].Price
	}
	if len(asks) > 0 {
		t.BestAsk = asks[0].Price
	}
	t.Timestamp = now
}

// Snapshot 取某交易对行情（不存在返回 nil）。
func (m *Market) Snapshot(symbol string) *Ticker {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if t, ok := m.tickers[symbol]; ok {
		cp := *t
		return &cp
	}
	return nil
}

// AllTickers 返回全部已知交易对的行情快照（symbol 为空时使用）。
func (m *Market) AllTickers() []Ticker {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Ticker, 0, len(m.tickers))
	for _, t := range m.tickers {
		out = append(out, *t)
	}
	return out
}

// RecentTrades 返回某交易对近期成交（最多 limit 条，按时间升序；limit<=0 取默认）。
func (m *Market) RecentTrades(symbol string, limit int) []*RecentTrade {
	if limit <= 0 || limit > recentTradesCap {
		limit = recentTradesCap
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := m.recentTrades[symbol]
	if len(list) > limit {
		list = list[len(list)-limit:]
	}
	out := make([]*RecentTrade, len(list))
	copy(out, list)
	return out
}

// Depth 取某交易对盘口快照（不存在返回 nil）。
func (m *Market) Depth(symbol string) *Depth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if d, ok := m.depths[symbol]; ok {
		cp := *d
		cp.Bids = append([]mq.DepthLevel(nil), d.Bids...)
		cp.Asks = append([]mq.DepthLevel(nil), d.Asks...)
		return &cp
	}
	return nil
}

// CurrentCandle 返回某交易对某周期的当前（未收盘）K 线（不存在返回 nil）。
func (m *Market) CurrentCandle(symbol, interval string) *Candle {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if iv, ok := m.klines[symbol]; ok {
		if c, ok := iv[interval]; ok {
			cp := *c
			return &cp
		}
	}
	return nil
}

// Klines 返回某交易对某周期的 K 线序列（已收盘历史在前，当前未收盘根在末尾），
// 按 open_time 升序；limit<=0 或超出上限时截断到 klineCap 且只保留末尾 limit 根。
//
// 当请求条数超出内存环形缓冲（klineCap）且装配了持久化层 Store 时，会从 Store 回取
// 更早的历史补足（[0, 内存最早根) 区间），与内存中的近期 K 线合并后截断到 limit；
// Store 不可用（未配置或查询失败）时降级为纯内存结果（fail-degraded）。
func (m *Market) Klines(symbol, interval string, limit int) []*Candle {
	if limit <= 0 || limit > klineCap {
		limit = klineCap
	}
	m.mu.RLock()
	hist := m.klineHistory[symbol][interval]
	// 内存最早根的 open_time 作为向 Store 回取的右开区间（避免与内存结果重复）。
	end := int64(0)
	if len(hist) > 0 {
		end = hist[0].OpenTime
	} else if cur, ok := m.klines[symbol][interval]; ok {
		end = cur.OpenTime
	}
	out := make([]*Candle, 0, len(hist)+1)
	for _, c := range hist {
		cp := *c
		out = append(out, &cp)
	}
	if cur, ok := m.klines[symbol][interval]; ok {
		cp := *cur
		out = append(out, &cp)
	}
	m.mu.RUnlock()

	// 扩展历史：内存不足且配置了持久化层时，回取更早的已收盘 K 线补足。
	if m.Store != nil && len(out) < limit {
		need := limit - len(out)
		qctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		older, err := m.Store.Query(qctx, symbol, interval, 0, end, int64(need))
		cancel()
		if err == nil && len(older) > 0 {
			merged := make([]*Candle, 0, len(older)+len(out))
			for _, c := range older {
				merged = append(merged, fromInfluxCandle(c))
			}
			merged = append(merged, out...)
			out = merged
		}
		// 查询失败：静默降级为内存结果（fail-degraded）。
	}

	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Symbols 返回所有已知交易对。
func (m *Market) Symbols() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.tickers))
	for s := range m.tickers {
		out = append(out, s)
	}
	return out
}

// capLevels 取前 n 档（调用方已按价格排好序：买盘高→低、卖盘低→高）。
func capLevels(levels []mq.DepthLevel, n int) []mq.DepthLevel {
	if len(levels) > n {
		return levels[:n]
	}
	return levels
}

// toInfluxCandle 把内存 K 线转换为持久化表示（字段对齐，避免 market <-> influxdb 循环依赖）。
func toInfluxCandle(c *Candle) influxdb.Candle {
	return influxdb.Candle{
		Symbol:          c.Symbol,
		Interval:        c.Interval,
		OpenTime:        c.OpenTime,
		Open:            c.Open,
		High:            c.High,
		Low:             c.Low,
		Close:           c.Close,
		Volume:          c.Volume,
		QuoteVolume:     c.QuoteVolume,
		BuyVolume:       c.BuyVolume,
		SellVolume:      c.SellVolume,
		BuyQuoteVolume:  c.BuyQuoteVolume,
		SellQuoteVolume: c.SellQuoteVolume,
		Ts:              c.Ts,
	}
}

// fromInfluxCandle 把持久化 K 线转换回内存表示。
func fromInfluxCandle(c *influxdb.Candle) *Candle {
	return &Candle{
		Symbol:          c.Symbol,
		Interval:        c.Interval,
		OpenTime:        c.OpenTime,
		Open:            c.Open,
		High:            c.High,
		Low:             c.Low,
		Close:           c.Close,
		Volume:          c.Volume,
		QuoteVolume:     c.QuoteVolume,
		BuyVolume:       c.BuyVolume,
		SellVolume:      c.SellVolume,
		BuyQuoteVolume:  c.BuyQuoteVolume,
		SellQuoteVolume: c.SellQuoteVolume,
		Ts:              c.Ts,
	}
}

// Close 释放行情存储占用的外部资源（如持久化层连接）；无 Store 时为 no-op。
func (m *Market) Close() error {
	if m.Store != nil {
		return m.Store.Close()
	}
	return nil
}
