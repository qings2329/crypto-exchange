package adminapi

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// handleAdminOrders 跨用户查询订单（运营风控用）。
// 支持过滤：user_id（不传=全部用户）、symbol、status、market（spot|futures）、limit。
func (s *Server) handleAdminOrders(c *gin.Context) {
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	symbol := c.Query("symbol")
	status := c.Query("status")
	market := c.Query("market")
	margin := c.Query("margin")
	limit, _ := strconv.Atoi(c.Query("limit"))

	// user_id=0 时引擎返回全部用户订单。
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
	if limit > 0 && len(orders) > limit {
		orders = orders[:limit]
	}
	response.JSON(c, gin.H{"orders": orders, "total": len(orders)})
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
// 支持过滤：user_id（不传=全部用户）、symbol、market（spot|futures）、limit。
func (s *Server) handleAdminTrades(c *gin.Context) {
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	symbol := c.Query("symbol")
	market := c.Query("market")
	limit, _ := strconv.Atoi(c.Query("limit"))

	all := s.matchClient.ListTrades(userID, symbol, 0)
	trades := make([]matching.TradeView, 0, len(all))
	for _, v := range all {
		if market != "" && v.Market != market {
			continue
		}
		trades = append(trades, v)
	}
	if limit > 0 && len(trades) > limit {
		trades = trades[:limit]
	}
	response.JSON(c, gin.H{"trades": trades, "total": len(trades)})
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
