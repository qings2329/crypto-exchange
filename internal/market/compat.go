package market

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// BinanceKline 是面向前端（crypto-exchange-web）的币安风格 K 线视图：
// 字段名 t/o/h/l/c/v，与 dev mock（mock/kline-server.mjs）及前端 Kline 类型一致。
// 既有 Candle 规范字段为 open_time/open/high/...（供其他消费者与 InfluxDB 落盘），保持不动；
// 此处仅做一层只读适配，使前端无需改动即可对接生产行情服务。
type BinanceKline struct {
	T int64   `json:"t"` // 桶起点（unix 毫秒），对应 Candle.OpenTime
	O float64 `json:"o"`
	H float64 `json:"h"`
	L float64 `json:"l"`
	C float64 `json:"c"`
	V float64 `json:"v"`
}

func toBinanceKline(c *Candle) BinanceKline {
	if c == nil {
		return BinanceKline{}
	}
	return BinanceKline{T: c.OpenTime, O: c.Open, H: c.High, L: c.Low, C: c.Close, V: c.Volume}
}

// klineSub 维护一个前端兼容 WS 订阅：仅当 symbol+interval 匹配时才推送币安风格 K 线。
type klineSub struct {
	symbol   string
	interval string
	ch       chan BinanceKline
}

// handleKlineCompat 是 /api/v1/market/kline（单数）的兼容别名：
// 入参与校验与 handleKlines 完全一致，但返回币安风格 K 线数组（t/o/h/l/c/v），
// 使 crypto-exchange-web 的 getKline 无需改动即可解析。
func (s *Server) handleKlineCompat(c *gin.Context) {
	sym := c.Query("symbol")
	if sym == "" {
		c.JSON(400, gin.H{"error": "symbol required"})
		return
	}
	interval := c.Query("interval")
	if interval == "" {
		interval = "1m"
	}
	if !IsValidInterval(interval) {
		c.JSON(400, gin.H{"error": "invalid interval", "valid": Intervals})
		return
	}
	limit := atoiDefault(c.Query("limit"), 100)
	candles := s.market.Klines(sym, interval, limit)
	out := make([]BinanceKline, 0, len(candles))
	for _, cd := range candles {
		out = append(out, toBinanceKline(cd))
	}
	c.JSON(200, out)
}

// handleKlineWSCompat 是 /api/v1/market/kline/ws 的兼容端点：
// 连接即按 ?symbol=&interval= 过滤（interval 支持逗号分隔的多周期，与 mock 网关一致），
// 初始推送各周期的当前蜡烛，之后每笔成交仅把匹配周期的 Candle 转成币安风格推送给本连接。
// 与既有 /ws（Hub + 全周期广播）协议不同，但互不干扰：本端点独立维护订阅表。
func (s *Server) handleKlineWSCompat(c *gin.Context) {
	sym := c.Query("symbol")
	if sym == "" {
		c.JSON(400, gin.H{"error": "symbol required"})
		return
	}
	rawInterval := c.Query("interval")
	if rawInterval == "" {
		rawInterval = "1m"
	}
	intervals := strings.Split(rawInterval, ",")
	for i, iv := range intervals {
		intervals[i] = strings.TrimSpace(iv)
	}
	// 过滤无效周期；全部无效时回退 1m
	valid := intervals[:0]
	for _, iv := range intervals {
		if iv == "" {
			continue
		}
		if !IsValidInterval(iv) {
			continue
		}
		valid = append(valid, iv)
	}
	if len(valid) == 0 {
		c.JSON(400, gin.H{"error": "invalid interval", "valid": Intervals})
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := up.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	// 为每个周期建立独立子订阅；同一 conn 可复用，dispatch 时按需投递。
	type connSub struct {
		sub  *klineSub
		sent int64 // 上次推送时间，用于去重（同 candle 不重复推送）
	}
	conns := make([]connSub, 0, len(valid))
	for _, iv := range valid {
		if cur := s.market.CurrentCandle(sym, iv); cur != nil {
			bk := toBinanceKline(cur)
			_ = conn.WriteJSON(gin.H{"type": "kline", "symbol": sym, "interval": iv, "data": bk})
		}
		sub := &klineSub{symbol: sym, interval: iv, ch: make(chan BinanceKline, 64)}
		s.klineSubsMu.Lock()
		s.klineSubs = append(s.klineSubs, sub)
		s.klineSubsMu.Unlock()
		conns = append(conns, connSub{sub: sub, sent: 0})
	}
	cleaned := conns // 闭包捕获
	// 每周期独立协程：收到新蜡烛即推送给 conn，携带 interval 字段供前端路由。
	done := make(chan struct{})
	defer func() {
		close(done) // 通知所有协程退出
		s.klineSubsMu.Lock()
		for _, cs := range cleaned {
			for i, x := range s.klineSubs {
				if x == cs.sub {
					s.klineSubs = append(s.klineSubs[:i], s.klineSubs[i+1:]...)
					break
				}
			}
		}
		s.klineSubsMu.Unlock()
		_ = conn.Close()
	}()
	for _, cs := range conns {
		go func(sub *klineSub, iv string) {
			for bk := range sub.ch {
				select {
				case <-done:
					return
				default:
				}
				if err := conn.WriteJSON(gin.H{"type": "kline", "symbol": sym, "interval": iv, "data": bk}); err != nil {
					return
				}
			}
		}(cs.sub, cs.sub.interval)
	}
	<-done
}

// dispatchKlineCompat 在每笔成交后，把匹配 symbol+interval 的当前蜡烛推送给前端兼容订阅者。
// 无订阅时（生产常态）直接返回，不影响既有 Hub 广播热路径。
func (s *Server) dispatchKlineCompat(symbol string) {
	s.klineSubsMu.RLock()
	subs := s.klineSubs
	s.klineSubsMu.RUnlock()
	if len(subs) == 0 {
		return
	}
	for _, iv := range Intervals {
		c := s.market.CurrentCandle(symbol, iv)
		if c == nil {
			continue
		}
		bk := toBinanceKline(c)
		for _, sub := range subs {
			if sub.symbol == symbol && sub.interval == iv {
				select {
				case sub.ch <- bk:
				default: // 发送缓冲满则丢弃，避免阻塞撮合
				}
			}
		}
	}
}
