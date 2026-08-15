package futuresapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// RegisterRoutes 注册合约交易服务全部 HTTP 路由。
// 注意：路由仅做参数绑定、调用领域依赖（ledger/futures/settlement/oracle）、返回响应，
// 不含业务规则（业务规则在 ledger 与 internal/futures 引擎内）。
func (s *Server) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	// 健康检查（免鉴权，供管理后台 / 网关 / 探针探活）。
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Unix()})
	})
	// 除 Prometheus 指标端点外，所有交易/钱包/风控接口均强制鉴权（含下单与提现）。
	r.Use(middleware.AuthWithSkips(verifier, "/metrics"))
	// 交易 / 行情 / 资金费
	r.POST("/api/v1/futures/order", s.handleOrder)
	r.GET("/api/v1/futures/positions", s.handlePositions)
	r.GET("/api/v1/futures/liquidations", s.handleLiquidations)
	r.GET("/api/v1/futures/adl", s.handleADL)
	r.GET("/api/v1/futures/socialized", s.handleSocialized)
	r.GET("/api/v1/futures/index", s.handleIndex)
	r.GET("/api/v1/futures/funding", s.handleFunding)
	r.GET("/api/v1/futures/funding-history", s.handleFundingHistory)
	r.GET("/api/v1/futures/ws", s.handleWS)
	// 用户侧订单管理：仅返回鉴权用户本人的合约订单/成交（按 token 中的 uid + market=futures 过滤）。
	r.GET("/api/v1/futures/orders", s.handleOrders)
	r.GET("/api/v1/futures/orders/:id", s.handleOrderDetail)
	r.GET("/api/v1/futures/trades", s.handleTrades)
	r.POST("/api/v1/futures/cancel", s.handleCancel)

	// 钱包 / 风控 / 指标（见 handler_wallet.go）
	s.registerWalletRoutes(r)
}

// handleOrder 开/平仓：action=open 开仓，action=close 平仓（骨架仅支持整仓反向市价/限价）。
func (s *Server) handleOrder(c *gin.Context) {
	var req struct {
		Symbol     string  `json:"symbol"`
		UserID     int64   `json:"user_id"`
		Action     string  `json:"action"` // open | close
		PosSide    string  `json:"pos_side"`
		MarginMode string  `json:"margin_mode"` // isolated（默认）| cross
		Leverage   float64 `json:"leverage"`
		Price      float64 `json:"price"`
		Qty        float64 `json:"qty"`
		Margin     float64 `json:"margin"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	// 坏账风控：有未冲抵坏账的用户禁止开立新仓（与限提出金对称），强制先补缴/回收。
	if req.Action == "open" && s.ledgerSvc.IsOutflowRestricted(req.UserID, "USDT") {
		response.Error(c, 403, 403, "opening blocked: repay outstanding bad debt first")
		return
	}
	ps := futures.Long
	if req.PosSide == "short" {
		ps = futures.Short
	}
	// 映射持仓方向到撮合买/卖：开多=买，开空=卖。
	side := matching.Buy
	if ps == futures.Short {
		side = matching.Sell
	}
	o := &matching.Order{
		UserID: req.UserID,
		Side:   side,
		Price:  req.Price,
		Qty:    req.Qty,
		Time:   time.Now().UnixNano(),
		Market: "futures",
		// 合约单本质为杠杆单：标记 IsMargin 并透传杠杆倍数，供订单管理按杠杆过滤。
		IsMargin: true,
		Leverage: req.Leverage,
	}
	if !s.matcher.Submit(req.Symbol, o) {
		response.Error(c, 400, 400, "unknown symbol or matching unavailable")
		return
	}
	if req.Action == "open" && req.Price > 0 {
		if book, ok := s.liquidator.Book(req.Symbol); ok {
			lev := req.Leverage
			if lev <= 0 {
				lev = 10
			}
			margin := req.Margin
			if margin <= 0 {
				margin = req.Price * req.Qty / lev
			}
			mode := futures.Isolated
			if req.MarginMode == "cross" {
				mode = futures.Cross
			}
			// 资金闭环：开仓前从钱包冻结保证金；余额不足则拒绝开仓。
			if err := s.ledgerSvc.Freeze(req.UserID, "USDT", margin); err != nil {
				response.Error(c, 400, 400, "insufficient margin: "+err.Error())
				return
			}
			if mode == futures.Cross {
				s.liquidator.OpenCross(req.Symbol, req.UserID, ps, req.Qty, req.Price, margin, lev, time.Now().UnixNano())
			} else {
				book.Open(req.UserID, req.Symbol, ps, req.Qty, req.Price, margin, lev, time.Now().UnixNano())
			}
		}
	}
	response.JSON(c, gin.H{"order_id": o.ID, "status": "accepted"})
}

// handleOrders 返回当前用户本人的合约订单列表，可按 symbol/status 过滤、limit 分页。
// 现货/合约共用同一撮合引擎登记簿，这里按 market=futures 过滤仅返回合约订单。
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
	all := s.matcher.ListOrders(uid, symbol, status, 0)
	orders := make([]matching.OrderView, 0, len(all))
	for _, v := range all {
		if v.Market != "futures" {
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

// handleOrderDetail 返回某笔合约订单详情；仅允许查看本人订单。
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
	v, ok2 := s.matcher.GetOrder(id)
	if !ok2 {
		response.Error(c, 404, 404, "not found")
		return
	}
	if v.UserID != uid || v.Market != "futures" {
		response.Error(c, 403, 403, "forbidden")
		return
	}
	response.JSON(c, gin.H{"order": v})
}

// handleTrades 返回当前用户本人的合约成交流水，可按 symbol 过滤、limit 分页。
// 按 market=futures 过滤，仅返回合约成交（现货/合约成交在统一登记簿中需区分）。
func (s *Server) handleTrades(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	symbol := c.Query("symbol")
	margin := c.Query("margin")
	limit, _ := strconv.Atoi(c.Query("limit"))
	all := s.matcher.ListTrades(uid, symbol, 0)
	trades := make([]matching.TradeView, 0, len(all))
	for _, v := range all {
		if v.Market != "futures" {
			continue
		}
		if !v.MarginMatches(margin) {
			continue
		}
		trades = append(trades, v)
	}
	if limit > 0 && len(trades) > limit {
		trades = trades[:limit]
	}
	response.JSON(c, gin.H{"trades": trades})
}

// handleCancel 撤销一笔合约订单；仅允许撤销本人订单，并校验归属。
func (s *Server) handleCancel(c *gin.Context) {
	var req struct {
		Symbol  string `json:"symbol"`
		OrderID int64  `json:"order_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	if v, ok2 := s.matcher.GetOrder(req.OrderID); ok2 {
		if v.UserID != uid || v.Market != "futures" {
			response.Error(c, 403, 403, "forbidden")
			return
		}
	}
	canceled := s.matcher.Cancel(req.Symbol, req.OrderID)
	response.JSON(c, gin.H{"symbol": req.Symbol, "order_id": req.OrderID, "canceled": canceled})
}

// handlePositions 查询某交易对的逐仓+全仓持仓，附带全仓用户的共享保证金余额。
func (s *Server) handlePositions(c *gin.Context) {
	symbol := c.Query("symbol")
	book, ok := s.liquidator.Book(symbol)
	if !ok {
		response.Error(c, 400, 400, "unknown symbol")
		return
	}
	crossBalances := make(map[string]float64)
	positions := s.liquidator.AllPositions(symbol)
	for _, p := range positions {
		if p.Mode == futures.Cross {
			crossBalances[fmt.Sprintf("%d", p.UserID)] = s.liquidator.CrossBalance(symbol, p.UserID)
		}
	}
	response.JSON(c, gin.H{
		"mark_price":     book.MarkPrice(),
		"positions":      positions,
		"cross_balances": crossBalances,
	})
}

func (s *Server) handleLiquidations(c *gin.Context) {
	response.JSON(c, gin.H{"liquidations": s.liquidator.RecentLiquidations()})
}

func (s *Server) handleADL(c *gin.Context) {
	response.JSON(c, gin.H{"adl": s.liquidator.RecentADL()})
}

func (s *Server) handleSocialized(c *gin.Context) {
	response.JSON(c, gin.H{"socialized": s.liquidator.RecentSocialized()})
}

// handleIndex 返回预言机聚合指数价与原始喂价样本。
func (s *Server) handleIndex(c *gin.Context) {
	response.JSON(c, gin.H{
		"index_prices": s.oracleSvc.Snapshot(),
		"raw_samples":  s.oracleSvc.RawSnapshot(),
	})
}

// handleFunding 返回某交易对的指数价/标记价/溢价 EMA/资金费率。
func (s *Server) handleFunding(c *gin.Context) {
	symbol := c.Query("symbol")
	mc, ok := s.markCalcs[symbol]
	if !ok {
		response.Error(c, 400, 400, "unknown symbol")
		return
	}
	index, _, lastRate, _ := s.funding.State(symbol)
	premium := mc.PremiumEMA()
	response.JSON(c, gin.H{
		"symbol":           symbol,
		"index_price":      index,
		"mark_price":       mc.MarkPrice(),
		"premium_ema":      premium,
		"funding_rate":     futures.FundingRate(futures.InterestRatePerInterval, premium),
		"last_settle_rate": lastRate,
		"funding_interval": s.funding.Interval().Seconds(),
	})
}

func (s *Server) handleFundingHistory(c *gin.Context) {
	response.JSON(c, gin.H{"funding": s.funding.RecentFunding()})
}

// handleWS 升级为行情 WebSocket，按 symbol 订阅成交/强平/资金费等推送。
func (s *Server) handleWS(c *gin.Context) {
	s.hub.Handle(c.Writer, c.Request)
}

// walletSummary 是 /wallet 余额查询的共用辅助（供 handler_wallet.go 复用）。
func (s *Server) walletSummary(uid int64, asset string) gin.H {
	avail, frozen, ok := s.ledgerSvc.Balance(uid, asset)
	wf, _ := s.ledgerSvc.WithdrawFrozenBalance(uid, asset)
	return gin.H{
		"user_id":         uid,
		"asset":           asset,
		"available":       avail,
		"frozen":          frozen,
		"withdraw_frozen": wf,
		"exists":          ok,
	}
}
