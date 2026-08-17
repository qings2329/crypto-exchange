package wealth

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// RegisterRoutes 在 gin 引擎上注册理财资管路由。
// 业务路由受 middleware.Auth(verifier) 保护（需合法 HMAC Bearer Token）。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/wealth")
	api.Use(middleware.Auth(verifier))
	{
		api.GET("/products", s.handleListProducts)
		api.POST("/products", middleware.AdminGuard(), s.handleCreateProduct)
		api.POST("/subscribe", s.handleSubscribe)
		api.POST("/redeem", s.handleRedeem)
		api.GET("/holdings", s.handleMyHoldings)
		api.GET("/admin/holdings", middleware.AdminGuard(), s.handleAdminHoldings)
		api.POST("/admin/accrue", middleware.AdminGuard(), s.handleAccrue)
	}
}

type createProductReq struct {
	Name         string  `json:"name"`
	Asset        string  `json:"asset"`
	Type         string  `json:"type"`
	AnnualRate   float64 `json:"annual_rate"`
	DurationDays int     `json:"duration_days"`
	MinAmount    float64 `json:"min_amount"`
}

func (s *Service) handleCreateProduct(c *gin.Context) {
	var req createProductReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.Type != "current" && req.Type != "fixed" {
		response.Error(c, 400, 4001, "type must be current or fixed")
		return
	}
	if req.AnnualRate < 0 {
		response.Error(c, 400, 4001, "annual_rate must be >= 0")
		return
	}
	if req.DurationDays < 0 {
		response.Error(c, 400, 4001, "duration_days must be >= 0")
		return
	}
	if req.Asset != "" && !settlement.KnownAsset(req.Asset) {
		response.Error(c, 400, 4001, "unsupported asset")
		return
	}
	if req.AnnualRate > MaxAnnualRate {
		response.Error(c, 400, 4001, "annual_rate exceeds maximum")
		return
	}
	p := &WealthProduct{
		Name:         req.Name,
		Asset:        req.Asset,
		Type:         ProductType(req.Type),
		AnnualRate:   req.AnnualRate,
		DurationDays: req.DurationDays,
		MinAmount:    req.MinAmount,
	}
	if err := s.CreateProduct(p); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, p)
}

func (s *Service) handleListProducts(c *gin.Context) {
	status := ProductStatus(c.Query("status"))
	list, err := s.ListProducts(status)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"products": list})
}

type subscribeReq struct {
	ProductID int64   `json:"product_id"`
	Amount    float64 `json:"amount"`
}

func (s *Service) handleSubscribe(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req subscribeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.ProductID <= 0 || req.Amount <= 0 {
		response.Error(c, 400, 4001, "product_id/amount required")
		return
	}
	h, err := s.Subscribe(uid, req.ProductID, req.Amount)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, h)
}

type redeemReq struct {
	HoldingID int64 `json:"holding_id"`
}

func (s *Service) handleRedeem(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req redeemReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.HoldingID <= 0 {
		response.Error(c, 400, 4001, "holding_id required")
		return
	}
	h, err := s.Redeem(uid, req.HoldingID)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, h)
}

func (s *Service) handleMyHoldings(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.MyHoldings(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"holdings": list})
}

func (s *Service) handleAdminHoldings(c *gin.Context) {
	list, err := s.AdminListHoldings()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"holdings": list})
}

func (s *Service) handleAccrue(c *gin.Context) {
	total, err := s.Accrue(time.Now())
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"accrued": total})
}
