package margin

import (
	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// RegisterRoutes 在 gin 引擎上注册杠杆业务路由。
// 业务路由受 middleware.Auth(verifier) 保护（需合法 HMAC Bearer Token）。
// 用户态端点一律以 token 身份为准（防 IDOR）；强制清算为运营动作，需 AdminGuard。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/margin")
	api.Use(middleware.Auth(verifier))
	{
		api.POST("/borrow", s.handleBorrow)
		api.POST("/repay", s.handleRepay)
		api.POST("/liquidate", middleware.AdminGuard(), s.handleLiquidate)
		api.POST("/accrue", s.handleAccrue)
		api.GET("/account", s.handleAccount)
		api.GET("/accounts", s.handleAccounts)
		api.GET("/liq-price", s.handleLiqPrice)
	}
}

type borrowReq struct {
	Asset    string  `json:"asset"`
	Amount   float64 `json:"amount"`
	Leverage int     `json:"leverage"`
}

func (s *Service) handleBorrow(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req borrowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.Asset == "" || req.Amount <= 0 || req.Leverage < 1 {
		response.Error(c, 400, 4001, "asset/amount/leverage required")
		return
	}
	a, err := s.Borrow(uid, req.Asset, req.Amount, req.Leverage)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, a)
}

type repayReq struct {
	Asset  string  `json:"asset"`
	Amount float64 `json:"amount"`
}

func (s *Service) handleRepay(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req repayReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.Asset == "" || req.Amount <= 0 {
		response.Error(c, 400, 4001, "asset/amount required")
		return
	}
	if err := s.Repay(uid, req.Asset, req.Amount); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

type adminLiqReq struct {
	UserID int64  `json:"user_id"` // 目标用户（仅管理员可指定）
	Asset  string `json:"asset"`
}

func (s *Service) handleLiquidate(c *gin.Context) {
	// 已受 AdminGuard 保护：调用者须为管理员，目标用户由请求体指定。
	var req adminLiqReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.UserID <= 0 || req.Asset == "" {
		response.Error(c, 400, 4001, "user_id/asset required")
		return
	}
	done, err := s.Liquidate(req.UserID, req.Asset)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"liquidated": done})
}

type assetReq struct {
	Asset string `json:"asset"`
}

func (s *Service) handleAccrue(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req assetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.Asset == "" {
		response.Error(c, 400, 4001, "asset required")
		return
	}
	if err := s.Accrue(uid, req.Asset); err != nil {
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
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
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

// parseUserAsset 以 token 身份作为调用者（用户只能查看自己的杠杆账户），asset 取自 query。
func parseUserAsset(c *gin.Context) (int64, string, bool) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return 0, "", false
	}
	asset := c.Query("asset")
	if asset == "" {
		response.Error(c, 400, 4001, "asset required")
		return 0, "", false
	}
	return uid, asset, true
}
