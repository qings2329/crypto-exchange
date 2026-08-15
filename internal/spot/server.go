package spot

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
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
//
// 资金闭环（本里程碑新增）：下单前在 ledger 预冻结买方计价资产 / 卖方基础资产；每笔成交
// 经 settleFill 解冻已成交部分并转账（买方付计价、卖方付基础）；撤单释放剩余冻结。
type Server struct {
	log    *zap.Logger
	client *client.Client
	hub    *ws.Hub

	ledgerSvc  *ledger.Ledger // 现货自有复式记账账本（与合约服务各自独立实例，见计划说明）
	freezeMu   sync.Mutex
	openOrders map[int64]*freezeRec // orderID -> 预冻结记录，供成交递减与撤单释放

	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer 装配现货交易服务：创建撮合客户端并订阅行情 WebSocket，转发到前端 hub。
func NewServer(ledgerSvc *ledger.Ledger, cfg *config.Config, log *zap.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		log:        log,
		client:     client.New(cfg.Matching.URL),
		hub:        ws.NewHub(),
		ledgerSvc:  ledgerSvc,
		openOrders: make(map[int64]*freezeRec),
		ctx:        ctx,
		cancel:     cancel,
	}

	// 订阅 cmd/matching 的行情流（成交 + 深度），转发到本地前端 hub；成交同时驱动记账。
	go func() {
		if err := s.client.Watch(ctx, []string{"BTC_USDT", "ETH_USDT"},
			func(ev client.TradeEvent) {
				s.hub.Broadcast(ev.Symbol, gin.H{"type": "trade", "data": ev.Trade})
				// 成交记账：异常被吞掉，绝不影响行情推送与 WS 读取循环。
				func() {
					defer func() {
						if r := recover(); r != nil {
							s.log.Error("spot settleFill panic", zap.Any("recover", r))
						}
					}()
					if err := s.settleFill(ev.Symbol, ev.Trade); err != nil {
						s.log.Warn("spot settleFill failed", zap.String("symbol", ev.Symbol), zap.Error(err))
					}
				}()
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
// 下单/撤单接口受鉴权保护；行情深度与 WebSocket 行情为公开端点（豁免鉴权），由网关/前端直接消费。
func (s *Server) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	r.Use(middleware.AuthWithSkips(verifier, "/api/v1/spot/depth", "/api/v1/spot/ws"))
	r.POST("/api/v1/spot/order", s.handleOrder)
	r.POST("/api/v1/spot/cancel", s.handleCancel)
	r.GET("/api/v1/spot/depth", s.handleDepth)
	r.GET("/api/v1/spot/ws", s.handleWS)
	// 用户侧订单管理：仅返回鉴权用户本人的订单/成交（按 token 中的 uid 过滤）。
	r.GET("/api/v1/spot/orders", s.handleOrders)
	r.GET("/api/v1/spot/orders/:id", s.handleOrderDetail)
	r.GET("/api/v1/spot/trades", s.handleTrades)
}

// handleOrder 提交一笔现货订单（买/卖），经 cmd/matching 撮合，并在撮合前预冻结资金。
func (s *Server) handleOrder(c *gin.Context) {
	var req struct {
		Symbol   string  `json:"symbol"`
		UserID   int64   `json:"user_id"`
		Side     string  `json:"side"`
		Price    float64 `json:"price"`
		Qty      float64 `json:"qty"`
		IsMargin bool    `json:"is_margin"`          // 杠杆现货单（借币后下单）
		Leverage float64 `json:"leverage,omitempty"` // 杠杆倍数（is_margin 时有效）
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	if req.UserID == 0 {
		response.Error(c, 400, 400, "user_id required")
		return
	}
	if req.Qty <= 0 {
		response.Error(c, 400, 400, "qty must be positive")
		return
	}
	side := matching.Buy
	if req.Side == "sell" {
		side = matching.Sell
	}
	if _, _, ok := splitSymbol(req.Symbol); !ok {
		response.Error(c, 400, 400, "unsupported symbol")
		return
	}

	o := &matching.Order{
		UserID: req.UserID,
		Side:   side,
		Price:  req.Price,
		Qty:    req.Qty,
		Time:   time.Now().UnixNano(),
		Market: "spot",
		// 杠杆现货单标记为 IsMargin，倍数透传；用于订单管理按杠杆过滤。
		IsMargin: req.IsMargin,
		Leverage: req.Leverage,
	}

	// 预冻结：撮合前锁定买方计价资产 / 卖方基础资产，杜绝「超卖/超买」。
	rec, err := s.reserveOnOpen(req.UserID, side, req.Price, req.Qty, req.Symbol)
	if err != nil {
		response.Error(c, 400, 400, "insufficient balance: "+err.Error())
		return
	}

	if !s.client.Submit(req.Symbol, o) {
		// 撮合不可用，回滚预冻结，避免资金被错误锁定。
		s.releaseRemaining(rec)
		response.Error(c, 400, 400, "unknown symbol or matching unavailable")
		return
	}

	// 记录预冻结（绑定 orderID），供成交递减与撤单释放。
	s.freezeMu.Lock()
	s.openOrders[o.ID] = rec
	s.freezeMu.Unlock()

	response.JSON(c, gin.H{"order_id": o.ID, "status": "accepted"})
}

// reserveOnOpen 下单前预冻结资金，返回（尚未绑定 orderID 的）预冻结记录。
// 市价买单（price=0）无法预估花费，不下预冻结，仅做可用余额内的尽力结算。
func (s *Server) reserveOnOpen(userID int64, side matching.Side, price, qty float64, symbol string) (*freezeRec, error) {
	base, quote, ok := splitSymbol(symbol)
	if !ok {
		return nil, fmt.Errorf("unsupported symbol %s", symbol)
	}
	rec := &freezeRec{user: userID, side: side, symbol: symbol, base: base, quote: quote}
	var asset string
	var amt float64
	switch {
	case side == matching.Buy && price > 0:
		asset, amt = quote, price*qty
	case side == matching.Sell:
		asset, amt = base, qty
	}
	if amt > 0 {
		if err := s.ledgerSvc.Freeze(userID, asset, amt); err != nil {
			return nil, err
		}
	}
	if side == matching.Buy && price > 0 {
		rec.frozenQuote = amt
	} else if side == matching.Sell {
		rec.frozenBase = amt
	}
	return rec, nil
}

// releaseRemaining 释放一条预冻结记录中尚未成交的冻结资金（按资产区分冻结维度）。
func (s *Server) releaseRemaining(rec *freezeRec) {
	if rec == nil {
		return
	}
	if rec.frozenQuote > 0 {
		_ = s.ledgerSvc.Unfreeze(rec.user, rec.quote, rec.frozenQuote)
	}
	if rec.frozenBase > 0 {
		_ = s.ledgerSvc.Unfreeze(rec.user, rec.base, rec.frozenBase)
	}
}

// handleCancel 撤销一笔现货订单，并释放其尚未成交的预冻结资金。
func (s *Server) handleCancel(c *gin.Context) {
	var req struct {
		Symbol  string `json:"symbol"`
		OrderID int64  `json:"order_id"`
		UserID  int64  `json:"user_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}

	// 释放剩余预冻结（幂等：记录不存在则跳过，仅转发撤单到撮合引擎）。
	s.freezeMu.Lock()
	rec, ok := s.openOrders[req.OrderID]
	if ok {
		s.releaseRemaining(rec)
		delete(s.openOrders, req.OrderID)
	}
	s.freezeMu.Unlock()

	canceled, _ := s.client.CancelOrder(req.Symbol, req.OrderID)
	response.JSON(c, gin.H{"symbol": req.Symbol, "order_id": req.OrderID, "canceled": canceled})
}

// settleFill 结算一笔现货成交：买方支付计价资产、卖方支付基础资产，均经账本转账。
// 若对应订单有预冻结，则先释放已成交部分再转账，保证「冻结→解冻→划转」资金闭环。
func (s *Server) settleFill(symbol string, t matching.Trade) error {
	base, quote, ok := splitSymbol(symbol)
	if !ok {
		return fmt.Errorf("unsupported symbol %s", symbol)
	}
	cost := t.Price * t.Qty

	// 确定买卖方用户（TakerSide 决定哪一侧是买方）。
	var buyer, seller int64
	if t.TakerSide == matching.Buy {
		buyer, seller = t.TakerID, t.MakerID
	} else {
		buyer, seller = t.MakerID, t.TakerID
	}
	ref := fmt.Sprintf("spot:%s:%d", symbol, t.TakerOID)
	if t.TakerOID == 0 {
		ref = fmt.Sprintf("spot:%s:%d", symbol, t.MakerOID)
	}

	// 按订单维度查找本方预冻结记录，成交时递减。
	buyRec := s.lookupFreeze(t.TakerOID, t.MakerOID, matching.Buy)
	sellRec := s.lookupFreeze(t.TakerOID, t.MakerOID, matching.Sell)

	// 买方支付计价资产。
	if buyRec != nil {
		_ = s.ledgerSvc.Unfreeze(buyer, quote, cost)
		buyRec.frozenQuote -= cost
		if buyRec.frozenQuote < 0 {
			buyRec.frozenQuote = 0
		}
	}
	if err := s.ledgerSvc.Transfer(buyer, seller, quote, cost, "spot_trade", ref); err != nil {
		s.log.Error("spot settle buyer leg failed", zap.Int64("buyer", buyer), zap.Error(err))
		return err
	}

	// 卖方支付基础资产。
	if sellRec != nil {
		_ = s.ledgerSvc.Unfreeze(seller, base, t.Qty)
		sellRec.frozenBase -= t.Qty
		if sellRec.frozenBase < 0 {
			sellRec.frozenBase = 0
		}
	}
	if err := s.ledgerSvc.Transfer(seller, buyer, base, t.Qty, "spot_trade", ref); err != nil {
		s.log.Error("spot settle seller leg failed", zap.Int64("seller", seller), zap.Error(err))
		return err
	}

	// 完全成交的订单清理记录。
	s.maybeCleanup(t.TakerOID)
	s.maybeCleanup(t.MakerOID)
	return nil
}

// lookupFreeze 在预冻结记录中按订单 ID 与方向查找（taker/maker 任一命中即可）。
func (s *Server) lookupFreeze(takerOID, makerOID int64, side matching.Side) *freezeRec {
	s.freezeMu.Lock()
	defer s.freezeMu.Unlock()
	for _, oid := range []int64{takerOID, makerOID} {
		if rec, ok := s.openOrders[oid]; ok && rec.side == side {
			return rec
		}
	}
	return nil
}

// maybeCleanup 当一笔订单的预冻结已完全释放时，移除其记录。
func (s *Server) maybeCleanup(orderID int64) {
	if orderID == 0 {
		return
	}
	s.freezeMu.Lock()
	defer s.freezeMu.Unlock()
	rec, ok := s.openOrders[orderID]
	if !ok {
		return
	}
	if rec.frozenQuote <= 1e-9 && rec.frozenBase <= 1e-9 {
		delete(s.openOrders, orderID)
	}
}

// handleOrders 返回当前用户本人的现货订单列表，可按 symbol/status 过滤、limit 分页。
func (s *Server) handleOrders(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	symbol := c.Query("symbol")
	status := c.Query("status")
	margin := c.Query("margin")
	limit, _ := strconv.Atoi(c.Query("limit"))
	all := s.client.ListOrders(uid, symbol, status, 0)
	orders := make([]matching.OrderView, 0, len(all))
	for _, v := range all {
		if v.Market != "spot" {
			continue
		}
		if !v.MarginMatches(margin) {
			continue
		}
		orders = append(orders, v)
	}
	if limit > 0 && len(orders) > limit {
		orders = orders[:limit]
	}
	response.JSON(c, gin.H{"orders": orders})
}

// handleOrderDetail 返回某笔现货订单详情；仅允许查看本人订单。
func (s *Server) handleOrderDetail(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, 400, 400, "bad order id")
		return
	}
	v, ok2 := s.client.GetOrder(id)
	if !ok2 {
		response.Error(c, 404, 404, "not found")
		return
	}
	if v.UserID != uid {
		response.Error(c, 403, 403, "forbidden")
		return
	}
	response.JSON(c, gin.H{"order": v})
}

// handleTrades 返回当前用户本人的现货成交流水，可按 symbol 过滤、limit 分页。
// 按 market=spot 过滤，仅返回现货成交（合约成交在统一登记簿中需区分）。
func (s *Server) handleTrades(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	symbol := c.Query("symbol")
	limit, _ := strconv.Atoi(c.Query("limit"))
	all := s.client.ListTrades(uid, symbol, 0)
	trades := make([]matching.TradeView, 0, len(all))
	for _, v := range all {
		if v.Market == "spot" {
			trades = append(trades, v)
		}
	}
	if limit > 0 && len(trades) > limit {
		trades = trades[:limit]
	}
	response.JSON(c, gin.H{"trades": trades})
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
