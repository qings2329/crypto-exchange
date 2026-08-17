package adminapi

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// handleAdminOrders 跨用户查询订单（运营风控用）。
// 支持过滤：user_id（不传=全部用户）、symbol、status、market（spot|futures）、margin；
// 支持分页：limit（默认 50，上限 500）、offset（默认 0）。响应 {orders, total}。
func (s *Server) handleAdminOrders(c *gin.Context) {
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	symbol := c.Query("symbol")
	status := c.Query("status")
	market := c.Query("market")
	margin := c.Query("margin")
	limit, offset := parsePage(c)

	// user_id=0 时引擎返回全部用户订单（limit=0 表示不做引擎层截断）。
	all := s.matchClient.ListOrders(userID, symbol, status, 0)
	orders := make([]matching.OrderView, 0, len(all))
	for _, v := range all {
		if market != "" && v.Market != market {
			continue
		}
		if !v.MarginMatches(margin) {
			continue
		}
		orders = append(orders, v)
	}
	page, total := paginate(orders, limit, offset)
	response.JSON(c, gin.H{"orders": page, "total": total})
}

// handleAdminOrderDetail 按订单 ID 查询详情（跨用户）。
func (s *Server) handleAdminOrderDetail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, 400, 400, "bad order id")
		return
	}
	v, ok := s.matchClient.GetOrder(id)
	if !ok {
		response.Error(c, 404, 404, "not found")
		return
	}
	response.JSON(c, gin.H{"order": v})
}

// handleAdminTrades 跨用户查询成交流水。
// 支持过滤：user_id（不传=全部用户）、symbol、market（spot|futures）、margin；
// 支持分页：limit（默认 50，上限 500）、offset（默认 0）。响应 {trades, total}。
func (s *Server) handleAdminTrades(c *gin.Context) {
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	symbol := c.Query("symbol")
	market := c.Query("market")
	margin := c.Query("margin")
	limit, offset := parsePage(c)

	all := s.matchClient.ListTrades(userID, symbol, 0)
	trades := make([]matching.TradeView, 0, len(all))
	for _, v := range all {
		if market != "" && v.Market != market {
			continue
		}
		if !v.MarginMatches(margin) {
			continue
		}
		trades = append(trades, v)
	}
	page, total := paginate(trades, limit, offset)
	response.JSON(c, gin.H{"trades": page, "total": total})
}

// handleAdminCancelOrder 撤销任意用户的订单（高危，需 trade:manage 权限）。
// 请求体需包含 symbol（撮合引擎按 symbol 定位订单簿）。
func (s *Server) handleAdminCancelOrder(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, 400, 400, "bad order id")
		return
	}
	var req struct {
		Symbol string `json:"symbol"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Symbol == "" {
		response.Error(c, 400, 400, "symbol required")
		return
	}
	canceled := s.matchClient.Cancel(req.Symbol, id)
	response.JSON(c, gin.H{"order_id": id, "symbol": req.Symbol, "canceled": canceled})
}
