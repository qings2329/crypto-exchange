package margin

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// RegisterRoutes 在 gin 引擎上注册杠杆业务路由。
// 业务路由受 middleware.Auth(verifier) 保护（需合法 HMAC Bearer Token）。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/margin")
	api.Use(middleware.Auth(verifier))
	{
		api.POST("/borrow", s.handleBorrow)
		api.POST("/repay", s.handleRepay)
		api.POST("/liquidate", s.handleLiquidate)
		api.POST("/accrue", s.handleAccrue)
		api.GET("/account", s.handleAccount)
		api.GET("/accounts", s.handleAccounts)
		api.GET("/liq-price", s.handleLiqPrice)
	}
}

type borrowReq struct {
	UserID   int64   `json:"user_id"`
	Asset    string  `json:"asset"`
	Amount   float64 `json:"amount"`
	Leverage int     `json:"leverage"`
}

func (s *Service) handleBorrow(c *gin.Context) {
	var req borrowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.Asset == "" || req.Amount <= 0 || req.Leverage < 1 {
		response.Error(c, 400, 4001, "asset/amount/leverage required")
		return
	}
	a, err := s.Borrow(req.UserID, req.Asset, req.Amount, req.Leverage)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, a)
}

type repayReq struct {
	UserID int64   `json:"user_id"`
	Asset  string  `json:"asset"`
	Amount float64 `json:"amount"`
}

func (s *Service) handleRepay(c *gin.Context) {
	var req repayReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if err := s.Repay(req.UserID, req.Asset, req.Amount); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (s *Service) handleLiquidate(c *gin.Context) {
	var req repayReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	done, err := s.Liquidate(req.UserID, req.Asset)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"liquidated": done})
}

func (s *Service) handleAccrue(c *gin.Context) {
	var req repayReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if err := s.Accrue(req.UserID, req.Asset); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (s *Service) handleAccount(c *gin.Context) {
	uid, asset, ok := parseUserAsset(c)
	if !ok {
		return
	}
	a, err := s.Account(uid, asset)
	if err != nil {
		response.Error(c, 404, 4040, err.Error())
		return
	}
	response.JSON(c, a)
}

func (s *Service) handleAccounts(c *gin.Context) {
	uid, err := strconv.ParseInt(c.Query("user_id"), 10, 64)
	if err != nil || uid <= 0 {
		response.Error(c, 400, 4000, "user_id required")
		return
	}
	list, err := s.Accounts(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"accounts": list})
}

func (s *Service) handleLiqPrice(c *gin.Context) {
	uid, asset, ok := parseUserAsset(c)
	if !ok {
		return
	}
	price, err := s.LiquidationPrice(uid, asset)
	if err != nil {
		response.Error(c, 404, 4040, err.Error())
		return
	}
	response.JSON(c, gin.H{"user_id": uid, "asset": asset, "liq_price": price})
}

// parseUserAsset 从 query 解析 user_id 与 asset。
func parseUserAsset(c *gin.Context) (int64, string, bool) {
	uid, err := strconv.ParseInt(c.Query("user_id"), 10, 64)
	if err != nil || uid <= 0 {
		response.Error(c, 400, 4000, "user_id required")
		return 0, "", false
	}
	asset := c.Query("asset")
	if asset == "" {
		response.Error(c, 400, 4001, "asset required")
		return 0, "", false
	}
	return uid, asset, true
}
