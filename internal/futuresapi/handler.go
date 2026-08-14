package futuresapi

import (
	"fmt"
	"net/http"
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
