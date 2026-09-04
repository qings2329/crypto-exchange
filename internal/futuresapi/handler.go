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
	"github.com/coldlar/crypto-exchange/internal/settlement"
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
	// 补齐端点：地址簿白名单 / 内部划转 / 持仓 TP-SL（契约对齐 mock 网关）
	s.registerGapRoutes(r)
	// 管理端点：交易手续费模型刷新（供 adminapi 在 upsertSymbol 后调用）。
	r.PUT("/api/v1/futures/admin/trading-fees/refresh", middleware.AdminGuard(), s.HandleTradingFeeRefresh)
}

// handleOrder 开/平仓：action=open 开仓，action=close 平仓（骨架仅支持整仓反向市价/限价）。
// 身份强制取自 token（F4），忽略请求体 user_id，用户只能开/平自己的仓、冻结自己的保证金。
func (s *Server) handleOrder(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		Symbol     string  `json:"symbol"`
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
	if req.Action == "open" && s.ledgerSvc.IsOutflowRestricted(uid, "USDT") {
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
	// 边界定点化：价格/数量按交易对配置的 scale 对齐为 Fixed（消除浮点精度漂移）。
	// 注意 margin 由定点乘积导出，与送入撮合的订单价量一致，避免冻结额与成交价对不上。
	price := matching.FixedFromFloat(req.Price, s.cfg.PriceScale(req.Symbol))
	qty := matching.FixedFromFloat(req.Qty, s.cfg.QtyScale(req.Symbol))
	o := &matching.Order{
		UserID: uid,
		Side:   side,
		Price:  price,
		Qty:    qty,
		Time:   time.Now().UnixNano(),
		Market: "futures",
		// 合约单本质为杠杆单：标记 IsMargin 并透传杠杆倍数，供订单管理按杠杆过滤。
		IsMargin: true,
		Leverage: req.Leverage,
	}

	// 开/平分离：开仓单送入撮合引擎（成交后由 onTrade 驱动标记价/强平）；
	// 平仓单不经过公开订单簿，而是经 Liquidator.closer 真实成交并原地减仓，避免产生游离挂单。
	switch req.Action {
	case "open":
		// 杠杆钳制：不允许超过最大杠杆（前端滑条上限 125x 与服务端一致，防绕过前端抬高风险敞口）。
		// 0/负数视为未指定 → 默认 10x；超过 MaxLeverage 直接拒绝，与 margin 服务的 ErrOverMaxLeverage 语义对齐。
		maxLev := s.cfg.MaxLeverage()
		lev := req.Leverage
		if lev <= 0 {
			lev = 10
		}
		if lev > float64(maxLev) {
			response.Error(c, 400, 400, "leverage exceeds max "+strconv.Itoa(maxLev))
			return
		}
		req.Leverage = lev
		o.Leverage = lev
		if !s.matcher.Submit(req.Symbol, o) {
			response.Error(c, 400, 400, "unknown symbol or matching unavailable")
			return
		}
		if req.Price > 0 {
			if book, ok := s.liquidator.Book(req.Symbol); ok {
				// lev 已在外部完成默认(<=0→10x)与上限钳制(<=MaxLeverage)，此处直接复用。
				lev := req.Leverage
				margin := req.Margin
				if margin <= 0 {
					margin = price.Mul(qty).Float() / lev
				}
				mode := futures.Isolated
				if req.MarginMode == "cross" {
					mode = futures.Cross
				}
				// 资金闭环：开仓前从钱包冻结保证金；余额不足则拒绝开仓。
				// M5：margin 来自用户请求（或价格派生），须经 Safe 拦截 NaN/Inf，避免冻结 0 金额造成无抵押开仓。
				marginAmt, err := settlement.AssetAmountFromFloatSafe(margin, settlement.AssetDecimalsByName("USDT"))
				if err != nil {
					response.Error(c, 400, 400, "invalid margin: "+err.Error())
					return
				}
				if err := s.ledgerSvc.Freeze(uid, "USDT", marginAmt); err != nil {
					response.Error(c, 400, 400, "insufficient margin: "+err.Error())
					return
				}
				if mode == futures.Cross {
					s.liquidator.OpenCross(req.Symbol, uid, ps, req.Qty, req.Price, margin, lev, time.Now().UnixNano())
				} else {
					book.Open(uid, req.Symbol, ps, req.Qty, req.Price, margin, lev, time.Now().UnixNano())
				}
			}
		}
		response.JSON(c, gin.H{"order_id": o.ID, "status": "accepted"})

	case "close":
		// 平仓：经撮合引擎真实成交（复用 Liquidator.closer，与强平同源），原地减仓并据盈亏调整保证金；
		// 资金闭环（释放保证金 + 结算实现盈亏）在 settleClose 内统一完成。
		res := s.liquidator.ClosePosition(req.Symbol, uid, ps, req.Qty)
		if !res.OK {
			response.JSON(c, gin.H{
				"order_id":     o.ID,
				"status":       "rejected",
				"reason":       "no_open_position",
				"realized_pnl": 0,
			})
			return
		}
		s.settleClose(uid, req.Symbol, res)
		response.JSON(c, gin.H{
			"order_id":     o.ID,
			"status":       "accepted",
			"realized_pnl": res.Realized,
		})

	default:
		response.Error(c, 400, 400, "unknown action")
	}
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
	// 注入持仓止盈止损（TP/SL），契约对齐 PUT /futures/tpsl。
	positions = s.decorateWithTPSL(positions)
	response.JSON(c, gin.H{
		"mark_price":     book.MarkPrice(),
		"positions":      positions,
		"cross_balances": crossBalances,
	})
}

func (s *Server) handleLiquidations(c *gin.Context) {
	response.JSON(c, gin.H{"liquidations": s.liquidator.RecentLiquidations()})
}

// decorateWithTPSL 为持仓切片注入已设置的止盈止损（按 uid|symbol|side 维一键查找）。
func (s *Server) decorateWithTPSL(positions []futures.Position) []futures.Position {
	if len(positions) == 0 {
		return positions
	}
	s.tpslMu.Lock()
	defer s.tpslMu.Unlock()
	out := make([]futures.Position, 0, len(positions))
	for _, p := range positions {
		np := p
		if uidMap, ok := s.tpsl[p.UserID]; ok {
			if st, ok2 := uidMap[tpslKey(p.UserID, p.Symbol, sideName(p.Side))]; ok2 {
				np.TP = st.TP
				np.SL = st.SL
			}
		}
		out = append(out, np)
	}
	return out
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
