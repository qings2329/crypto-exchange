package market

import (
	"context"
	"encoding/json"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/client"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/es"
	"github.com/coldlar/crypto-exchange/internal/pkg/influxdb"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/ws"
)

// Server 聚合行情服务运行所需的依赖与生命周期。
type Server struct {
	log         *zap.Logger
	cfg         *config.Config
	market      *Market
	hub         *ws.Hub
	demoSymbols []string
	indexer     es.TradeIndexer // 可选：成交检索引擎（ES）；nil 时仅用内存

	// 前端（crypto-exchange-web）兼容层：按 symbol+interval 过滤并推送币安风格 K 线的订阅表。
	// 仅在有兼容 WS 连接时非空；热路径 applyTrade 在无订阅时快速跳过，不影响既有 Hub 广播。
	klineSubsMu sync.RWMutex
	klineSubs   []*klineSub

	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer 装配行情服务：创建行情存储、WebSocket Hub，并按配置选择数据源
// （Kafka 消费 / 撮合引擎 WebSocket 行情流 / 演示喂价兜底）。
func NewServer(cfg *config.Config, log *zap.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewMarket()
	hub := ws.NewHub()
	s := &Server{
		log:         log,
		cfg:         cfg,
		market:      m,
		hub:         hub,
		demoSymbols: []string{"BTCUSDT", "ETHUSDT"},
		ctx:         ctx,
		cancel:      cancel,
	}
	// 行情 K 线持久化（T-16）：配置了 InfluxDB 则落盘已收盘 K 线并支持超内存环形回取；
	// 未配置则仅用内存（fail-degraded）。
	if cfg.InfluxDB.URL != "" {
		m.SetCandleStore(influxdb.New(cfg.InfluxDB.URL, cfg.InfluxDB.Token, cfg.InfluxDB.Org, cfg.InfluxDB.Bucket))
		s.log.Info("market kline persistence: influxdb",
			zap.String("url", cfg.InfluxDB.URL), zap.String("bucket", cfg.InfluxDB.Bucket))
	} else {
		s.log.Info("market kline persistence: in-memory only (no influxdb configured)")
	}
	// 成交检索（T-16）：配置了 ES 则把每笔成交索引入 ES，支持历史成交检索；
	// 未配置则仅用内存（fail-degraded，检索端点降级为内存近期成交）。
	if cfg.ES.URL != "" {
		s.indexer = es.New(cfg.ES.URL, cfg.ES.Index)
		s.log.Info("market trade search: elasticsearch",
			zap.String("url", cfg.ES.URL), zap.String("index", cfg.ES.Index))
	} else {
		s.log.Info("market trade search: in-memory only (no elasticsearch configured)")
	}
	s.startSource()
	return s
}

// startSource 选择行情数据源：Kafka 优先，其次撮合引擎 WebSocket 行情流，最后演示喂价。
func (s *Server) startSource() {
	if len(s.cfg.Kafka.Brokers) > 0 {
		s.startKafkaSource()
		return
	}
	if s.cfg.Matching.URL != "" {
		s.startWSSource()
		return
	}
	s.log.Warn("market: no kafka brokers and no matching.url; falling back to demo feed")
	go s.demoFeed()
}

// startKafkaSource 通过 mq.Subscriber 消费 exchange.trades 与 market.depth，
// 按 topic 分发到行情聚合逻辑并广播 WebSocket。
func (s *Server) startKafkaSource() {
	handler := func(_ context.Context, topic string, data []byte) error {
		switch topic {
		case s.cfg.Kafka.TradeTopic:
			var ev mq.TradeEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				return err
			}
			s.applyTrade(ev)
		case s.cfg.Kafka.DepthTopic:
			var ev mq.DepthEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				return err
			}
			s.applyDepth(ev)
		}
		return nil
	}
	sub := mq.NewSubscriber(s.cfg.Kafka.Brokers, "market-group", handler)
	go func() {
		if err := sub.Subscribe(s.ctx, []string{s.cfg.Kafka.TradeTopic, s.cfg.Kafka.DepthTopic}, handler); err != nil && s.ctx.Err() == nil {
			s.log.Warn("market kafka subscribe exited", zap.Error(err))
		}
	}()
	s.log.Info("market source: kafka", zap.Strings("brokers", s.cfg.Kafka.Brokers))
}

// startWSSource 订阅 cmd/matching 的 WebSocket 行情流（与 spot 同构），驱动行情聚合。
func (s *Server) startWSSource() {
	symbols := s.cfg.Matching.Symbols
	if len(symbols) == 0 {
		symbols = []string{"BTC_USDT", "ETH_USDT", "BTC_USDT_PERP", "ETH_USDT_PERP"}
	}
	c := client.New(s.cfg.Matching.URL)
	go func() {
		err := c.Watch(s.ctx, symbols,
			func(ev client.TradeEvent) {
				takerSide := "buy"
				if ev.Trade.TakerSide == matching.Sell {
					takerSide = "sell"
				}
				s.applyTrade(mq.TradeEvent{
					Symbol:    ev.Symbol,
					Price:     ev.Trade.Price.Float(), // 定点→float64：mq 边界保持兼容
					Qty:       ev.Trade.Qty.Float(),
					TakerID:   ev.Trade.TakerID,
					MakerID:   ev.Trade.MakerID,
					TakerSide: takerSide,
					Ts:        time.Now().UnixMilli(),
				})
			},
			func(ev client.DepthEvent) {
				s.applyDepth(mq.DepthEvent{
					Symbol: ev.Symbol,
					Bids:   toDepthLevels(ev.Bids),
					Asks:   toDepthLevels(ev.Asks),
					Ts:     time.Now().UnixMilli(),
				})
			})
		if err != nil && s.ctx.Err() == nil {
			s.log.Warn("market matching watch exited", zap.Error(err))
		}
	}()
	s.log.Info("market source: matching websocket", zap.String("url", s.cfg.Matching.URL))
}

// applyTrade 聚合一笔成交并广播 trade/ticker/kline 三类 WebSocket 消息。
func (s *Server) applyTrade(ev mq.TradeEvent) {
	s.market.ApplyTrade(ev)
	s.hub.Broadcast(ev.Symbol, gin.H{"type": "trade", "symbol": ev.Symbol, "data": ev})
	if t := s.market.Snapshot(ev.Symbol); t != nil {
		s.hub.Broadcast(ev.Symbol, gin.H{"type": "ticker", "symbol": ev.Symbol, "data": t})
	}
	// 每笔成交推送各周期的当前（未收盘）K 线，与 ticker 同节奏；客户端按 interval 过滤。
	for _, iv := range Intervals {
		if c := s.market.CurrentCandle(ev.Symbol, iv); c != nil {
			s.hub.Broadcast(ev.Symbol, gin.H{"type": "kline", "symbol": ev.Symbol, "data": c})
		}
	}
	// 前端兼容（crypto-exchange-web）：按 symbol+interval 过滤后，推送币安风格 K 线。
	s.dispatchKlineCompat(ev.Symbol)
	// 成交检索（T-16）：异步把成交索引入 ES（best-effort，失败仅记录不阻断行情）。
	if s.indexer != nil {
		doc := es.TradeDoc{
			Symbol:    ev.Symbol,
			Price:     ev.Price,
			Qty:       ev.Qty,
			TakerID:   ev.TakerID,
			MakerID:   ev.MakerID,
			TakerSide: ev.TakerSide,
			Value:     ev.Price * ev.Qty,
			Ts:        ev.Ts,
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := s.indexer.Index(ctx, doc); err != nil {
				s.log.Warn("es index trade failed", zap.String("symbol", ev.Symbol), zap.Error(err))
			}
		}()
	}
}

// applyDepth 聚合一份深度快照并广播 depth 类 WebSocket 消息。
func (s *Server) applyDepth(ev mq.DepthEvent) {
	s.market.ApplyDepth(ev.Symbol, ev.Bids, ev.Asks)
	s.hub.Broadcast(ev.Symbol, gin.H{"type": "depth", "symbol": ev.Symbol, "data": ev})
}

// toDepthLevels 把撮合引擎的订单簿层级聚合为深度行（每档 volume = 各订单 Qty 之和，
// 定点求和后统一转 float64：mq 边界保持兼容）。
func toDepthLevels(levels []matching.Level) []mq.DepthLevel {
	out := make([]mq.DepthLevel, 0, len(levels))
	for _, l := range levels {
		var v matching.Fixed
		for _, o := range l.Orders {
			v = v.Add(o.Qty)
		}
		out = append(out, mq.DepthLevel{Price: l.Price.Float(), Volume: v.Float()})
	}
	return out
}

// demoFeed 演示随机游走喂价（仅作无 Kafka/无 matching URL 时的兜底）。
func (s *Server) demoFeed() {
	prices := map[string]float64{"BTCUSDT": 50000, "ETHUSDT": 3000}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			for _, sym := range s.demoSymbols {
				p := prices[sym] * (1 + (rand.Float64()-0.5)*0.002) // ±0.1% 随机游走
				prices[sym] = p
				s.applyTrade(mq.TradeEvent{Symbol: sym, Price: p, Qty: 1, Ts: time.Now().UnixMilli()})
			}
		}
	}
}

// RegisterRoutes 注册行情服务 HTTP 路由。
func (s *Server) RegisterRoutes(r *gin.Engine) {
	r.GET("/ws", s.handleWS)
	r.GET("/api/v1/market/ticker", s.handleTicker)
	r.GET("/api/v1/market/depth", s.handleDepth)
	r.GET("/api/v1/market/trades", s.handleTrades)
	r.GET("/api/v1/market/trades/search", s.handleTradeSearch)
	r.GET("/api/v1/market/klines", s.handleKlines)
	// 前端（crypto-exchange-web）兼容端点：单数 /kline 返回币安风格数组，
	// /kline/ws 提供按 symbol+interval 过滤的原始推送，使既有前端无需改动即可对接生产。
	r.GET("/api/v1/market/kline", s.handleKlineCompat)
	r.GET("/api/v1/market/kline/ws", s.handleKlineWSCompat)
}

// handleWS 升级为行情 WebSocket，按 symbol 订阅实时推送（trade/depth/ticker）。
func (s *Server) handleWS(c *gin.Context) {
	s.hub.Handle(c.Writer, c.Request)
}

// handleTicker 返回某交易对的实时行情；symbol 为空时返回全部。
func (s *Server) handleTicker(c *gin.Context) {
	sym := c.Query("symbol")
	if sym == "" {
		c.JSON(200, s.market.AllTickers())
		return
	}
	if t := s.market.Snapshot(sym); t != nil {
		c.JSON(200, t)
		return
	}
	c.JSON(200, gin.H{"symbol": sym, "last": 0, "timestamp": time.Now().UnixMilli()})
}

// handleDepth 返回某交易对的盘口快照（默认 depthCap 档，可用 limit 截断）。
func (s *Server) handleDepth(c *gin.Context) {
	sym := c.Query("symbol")
	if sym == "" {
		c.JSON(400, gin.H{"error": "symbol required"})
		return
	}
	d := s.market.Depth(sym)
	if d == nil {
		c.JSON(404, gin.H{"error": "no depth for symbol", "symbol": sym})
		return
	}
	limit := atoiDefault(c.Query("limit"), depthCap)
	if limit > 0 {
		if len(d.Bids) > limit {
			d.Bids = d.Bids[:limit]
		}
		if len(d.Asks) > limit {
			d.Asks = d.Asks[:limit]
		}
	}
	c.JSON(200, d)
}

// handleTrades 返回某交易对的近期成交（默认 50 条，上限 recentTradesCap）。
func (s *Server) handleTrades(c *gin.Context) {
	sym := c.Query("symbol")
	if sym == "" {
		c.JSON(400, gin.H{"error": "symbol required"})
		return
	}
	limit := atoiDefault(c.Query("limit"), 50)
	c.JSON(200, s.market.RecentTrades(sym, limit))
}

// handleTradeSearch 按条件检索成交历史（T-16 / ES）：支持 symbol / 买卖方向 / 时间窗 / 条数。
// 配置了 ES 时走全量历史检索（按 ts 降序）；未配置时降级为内存近期成交（按 symbol 过滤），
// 保证检索端点始终可用（fail-degraded）。
func (s *Server) handleTradeSearch(c *gin.Context) {
	q := es.TradeQuery{
		Symbol: c.Query("symbol"),
		Side:   c.Query("side"),
		Limit:  atoiDefault(c.Query("limit"), 100),
	}
	if v, err := strconv.ParseInt(c.Query("from"), 10, 64); err == nil {
		q.From = v
	}
	if v, err := strconv.ParseInt(c.Query("to"), 10, 64); err == nil {
		q.To = v
	}

	if s.indexer == nil {
		// 降级：未配置 ES 时返回内存近期成交（按 symbol 过滤；无 symbol 则要求必填）。
		if q.Symbol == "" {
			c.JSON(400, gin.H{"error": "symbol required (ES not enabled: full search unavailable)"})
			return
		}
		rt := s.market.RecentTrades(q.Symbol, q.Limit)
		out := make([]es.TradeDoc, 0, len(rt))
		for _, t := range rt {
			out = append(out, es.TradeDoc{
				Symbol: t.Symbol, Price: t.Price, Qty: t.Qty,
				TakerSide: t.Side, Ts: t.Ts, Value: t.Price * t.Qty,
			})
		}
		c.JSON(200, out)
		return
	}

	docs, err := s.indexer.Search(c.Request.Context(), q)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, docs)
}

// handleKlines 返回某交易对某周期的 K 线序列（已收盘历史 + 当前未收盘根，按 open_time 升序）。
func (s *Server) handleKlines(c *gin.Context) {
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
	c.JSON(200, s.market.Klines(sym, interval, limit))
}

// Close 停止数据源（取消上下文）并释放行情存储占用的外部资源（如 InfluxDB 连接）。
func (s *Server) Close() {
	_ = s.market.Close()
	s.cancel()
}

// atoiDefault 解析查询参数整数，失败或 <=0 时返回默认值。
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return def
	}
	return v
}
