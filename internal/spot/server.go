package spot

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/client"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/ws"
)

// Server 聚合现货交易服务运行所需的依赖与生命周期。
//
// 多实例收敛（见 DEVELOPMENT_TASKS §18）：现货不再持有进程内撮合引擎，而是改为调用
// 独立的 cmd/matching 服务（matching.Client 实现 matching.Matcher）。匹配权威唯一，
// 订单簿不再随现货进程分裂；成交流与深度由 cmd/matching 经 WebSocket 推送，本服务
// 仅负责转发到前端行情 hub。
type Server struct {
	log    *zap.Logger
	client *client.Client
	hub    *ws.Hub

	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer 装配现货交易服务：创建撮合客户端并订阅行情 WebSocket，转发到前端 hub。
func NewServer(cfg *config.Config, log *zap.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		log:    log,
		client: client.New(cfg.Matching.URL),
		hub:    ws.NewHub(),
		ctx:    ctx,
		cancel: cancel,
	}

	// 订阅 cmd/matching 的行情流（成交 + 深度），转发到本地前端 hub。
	go func() {
		if err := s.client.Watch(ctx, []string{"BTC_USDT", "ETH_USDT"},
			func(ev client.TradeEvent) {
				s.hub.Broadcast(ev.Symbol, gin.H{"type": "trade", "data": ev.Trade})
			},
			func(ev client.DepthEvent) {
				s.hub.Broadcast(ev.Symbol, gin.H{
					"type": "depth",
					"data": gin.H{"bids": aggregate(ev.Bids), "asks": aggregate(ev.Asks)},
				})
			}); err != nil && ctx.Err() == nil {
			s.log.Warn("spot matching watch exited", zap.Error(err))
		}
	}()

	return s
}

// RegisterRoutes 注册现货交易服务 HTTP 路由。
// 下单接口受鉴权保护；行情深度与 WebSocket 行情为公开端点（豁免鉴权），由网关/前端直接消费。
func (s *Server) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	r.Use(middleware.AuthWithSkips(verifier, "/api/v1/spot/depth", "/api/v1/spot/ws"))
	r.POST("/api/v1/spot/order", s.handleOrder)
	r.GET("/api/v1/spot/depth", s.handleDepth)
	r.GET("/api/v1/spot/ws", s.handleWS)
}

// handleOrder 提交一笔现货订单（买/卖），经 cmd/matching 撮合。
func (s *Server) handleOrder(c *gin.Context) {
	var req struct {
		Symbol string  `json:"symbol"`
		Side   string  `json:"side"`
		Price  float64 `json:"price"`
		Qty    float64 `json:"qty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	side := matching.Buy
	if req.Side == "sell" {
		side = matching.Sell
	}
	o := &matching.Order{
		Side:  side,
		Price: req.Price,
		Qty:   req.Qty,
		Time:  time.Now().UnixNano(),
	}
	if !s.client.Submit(req.Symbol, o) {
		response.Error(c, 400, 400, "unknown symbol or matching unavailable")
		return
	}
	response.JSON(c, gin.H{"order_id": o.ID, "status": "accepted"})
}

// handleDepth 返回某交易对的订单簿深度（来自 cmd/matching）。
func (s *Server) handleDepth(c *gin.Context) {
	symbol := c.Query("symbol")
	bids, asks, ok := s.client.Depth(symbol)
	if !ok {
		response.Error(c, 400, 400, "unknown symbol or matching unavailable")
		return
	}
	response.JSON(c, gin.H{"bids": aggregate(bids), "asks": aggregate(asks)})
}

// handleWS 升级为现货行情 WebSocket，推送成交与深度。
func (s *Server) handleWS(c *gin.Context) {
	s.hub.Handle(c.Writer, c.Request)
}

// Close 停止行情订阅。
func (s *Server) Close() {
	s.cancel()
}
