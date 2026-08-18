package staking

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// RegisterRoutes 在 gin 引擎上注册质押路由。用户接口受 middleware.Auth 保护，
// 产品创建/奖励归集/全量查询受 middleware.AdminGuard 保护（F4 鉴权守卫）。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/staking")
	api.Use(middleware.Auth(verifier))
	{
		api.GET("/products", s.handleListProducts)
		api.POST("/products", middleware.AdminGuard(), s.handleCreateProduct)
		api.POST("/subscribe", s.handleSubscribe)
		api.POST("/unbond", s.handleUnbond)
		api.POST("/release", s.handleRelease)
		api.GET("/holdings", s.handleMyDelegations)
		api.GET("/admin/holdings", middleware.AdminGuard(), s.handleAdminDelegations)
		api.POST("/admin/accrue", middleware.AdminGuard(), s.handleAccrue)
	}
}

type createProductReq struct {
	Name         string  `json:"name"`
	Chain        string  `json:"chain"`
	Validator    string  `json:"validator"`
	ContractAddr string  `json:"contract_addr"`
	Asset        string  `json:"asset"`
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
	if req.Asset != "" && !settlement.KnownAsset(req.Asset) {
		response.Error(c, 400, 4001, "unsupported asset")
		return
	}
	if req.AnnualRate < 0 {
		response.Error(c, 400, 4001, "annual_rate must be >= 0")
		return
	}
	dec := settlement.AssetDecimalsByName(req.Asset)
	p := &StakingProduct{
		Name:         req.Name,
		Chain:        req.Chain,
		Validator:    req.Validator,
		ContractAddr: req.ContractAddr,
		Asset:        req.Asset,
		AnnualRate:   req.AnnualRate,
		DurationDays: req.DurationDays,
		MinAmount:    settlement.AssetAmountFromFloat(req.MinAmount, dec),
		Status:       ProductActive,
	}
	if err := s.store.CreateProduct(p); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, p)
}

func (s *Service) handleListProducts(c *gin.Context) {
	list, err := s.store.ListProducts(ProductActive)
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
	p, err := s.store.GetProduct(req.ProductID)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	dec := settlement.AssetDecimalsByName(p.Asset)
	d, err := s.Subscribe(uid, req.ProductID, settlement.AssetAmountFromFloat(req.Amount, dec))
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, d)
}

type unbondReq struct {
	DelegationID int64 `json:"delegation_id"`
}

func (s *Service) handleUnbond(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req unbondReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.DelegationID <= 0 {
		response.Error(c, 400, 4001, "delegation_id required")
		return
	}
	d, err := s.Unbond(uid, req.DelegationID)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, d)
}

func (s *Service) handleRelease(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req unbondReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	d, err := s.Release(uid, req.DelegationID)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, d)
}

func (s *Service) handleMyDelegations(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.store.ListDelegationsByUser(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"delegations": list})
}

func (s *Service) handleAdminDelegations(c *gin.Context) {
	list, err := s.store.ListAllDelegations()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"delegations": list})
}

func (s *Service) handleAccrue(c *gin.Context) {
	total, err := s.Accrue(time.Now())
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"accrued": total})
}
