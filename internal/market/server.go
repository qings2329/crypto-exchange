package market

import (
	"context"
	"math/rand"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/ws"
)

// Server 聚合行情服务运行所需的依赖与生命周期。
type Server struct {
	log         *zap.Logger
	market      *Market
	hub         *ws.Hub
	publisher   mq.Publisher
	demoSymbols []string

	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer 装配行情服务：创建行情存储、WebSocket Hub、成交流订阅发布器，
// 并启动演示随机游走喂价。发布器的成交回调会更新行情并广播。
func NewServer(cfg *config.Config, log *zap.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewMarket()
	hub := ws.NewHub()
	s := &Server{
		log:         log,
		market:      m,
		hub:         hub,
		demoSymbols: []string{"BTCUSDT", "ETHUSDT"},
		ctx:         ctx,
		cancel:      cancel,
	}
	// 成交流发布器：若配置了 Kafka brokers 则订阅成交驱动行情；否则仅演示喂价。
	s.publisher = mq.NewPublisher(cfg.Kafka.Brokers, cfg.Kafka.TradeTopic, func(_ context.Context, ev mq.TradeEvent) {
		m.Update(ev)
		hub.Broadcast(ev.Symbol, m.Snapshot(ev.Symbol))
	})
	go s.demoFeed()
	return s
}

// demoFeed 演示随机游走喂价：为常见交易对生成实时行情，便于 WebSocket 直观验证。
// 生产应关闭，由真实成交流驱动。
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
				ev := mq.TradeEvent{Symbol: sym, Price: p, Qty: 1, Ts: time.Now().UnixMilli()}
				s.market.Update(ev)
				s.hub.Broadcast(sym, s.market.Snapshot(sym))
			}
		}
	}
}

// RegisterRoutes 注册行情服务 HTTP 路由。
func (s *Server) RegisterRoutes(r *gin.Engine) {
	r.GET("/ws", s.handleWS)
	r.GET("/api/v1/market/ticker", s.handleTicker)
}

// handleWS 升级为行情 WebSocket，按 symbol 订阅实时 ticker 推送。
func (s *Server) handleWS(c *gin.Context) {
	s.hub.Handle(c.Writer, c.Request)
}

// handleTicker 返回某交易对的实时行情快照。
func (s *Server) handleTicker(c *gin.Context) {
	sym := c.Query("symbol")
	if t := s.market.Snapshot(sym); t != nil {
		c.JSON(200, t)
		return
	}
	c.JSON(200, gin.H{"symbol": sym, "last": 0, "timestamp": time.Now().UnixMilli()})
}

// Close 停止演示喂价并关闭发布器。
func (s *Server) Close() {
	s.cancel()
	if s.publisher != nil {
		s.publisher.Close()
	}
}
