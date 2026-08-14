// Package market 是行情服务的「领域层 + HTTP Handler 层」。
//
// 分层定位：
//   - 领域对象 Market（内存行情存储）：维护各交易对的实时 ticker，并提供快照/交易对列表查询。
//   - 应用装配 Server：创建行情存储、WebSocket Hub、成交流订阅发布器，并启动演示随机游走喂价，
//     通过 RegisterRoutes 暴露 /ws 与 /api/v1/market/ticker。
//   - cmd/market/main.go 仅做进程级装配：读配置、建日志、调用 NewServer + RegisterRoutes + Run。
//
// 生产环境行情应由撮合成交流（Kafka）驱动 Market.Update；演示随机游走仅用于本地直观验证。
package market

import (
	"sync"

	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
)

// Ticker 某交易对的实时行情快照。
type Ticker struct {
	Symbol    string  `json:"symbol"`
	Last      float64 `json:"last"`
	BestBid   float64 `json:"best_bid"`
	BestAsk   float64 `json:"best_ask"`
	Timestamp int64   `json:"timestamp"`
}

// Market 维护各交易对的实时行情，供 WebSocket 推送与 REST 查询。
// 生产环境价格由撮合引擎/Kafka 成交流驱动（见 onTrade）；这里同时支持演示随机游走喂价。
type Market struct {
	mu      sync.RWMutex
	tickers map[string]*Ticker
}

// NewMarket 创建空的行情存储。
func NewMarket() *Market {
	return &Market{tickers: make(map[string]*Ticker)}
}

// Update 用一笔成交更新行情（买/卖价围绕成交价展开）。
func (m *Market) Update(ev mq.TradeEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickers[ev.Symbol]
	if !ok {
		t = &Ticker{Symbol: ev.Symbol}
		m.tickers[ev.Symbol] = t
	}
	t.Last = ev.Price
	// 简单买卖盘口：在成交价两侧挂 0.05% 价差（演示用，生产来自订单簿深度）。
	t.BestBid = ev.Price * 0.9995
	t.BestAsk = ev.Price * 1.0005
	t.Timestamp = ev.Ts
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
