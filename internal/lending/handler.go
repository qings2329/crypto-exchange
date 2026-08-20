package lending

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// RegisterRoutes 在 gin 引擎上注册借贷路由。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/lending")
	api.Use(middleware.Auth(verifier))
	{
		api.GET("/pools", s.handleListPools)
		api.GET("/pools/:id", s.handlePoolInfo)
		api.POST("/lend", s.handleLend)
		api.POST("/borrow", s.handleBorrow)
		api.POST("/repay/:id", s.handleRepay)
		api.POST("/withdraw/:id", s.handleWithdraw)
		api.GET("/my/lends", s.handleMyLends)
		api.GET("/my/borrows", s.handleMyBorrows)
		// 管理：全量池子/存款/借款查看 + 创建池（运维/风控用）。
		api.GET("/admin/pools", middleware.AdminGuard(), s.handleAdminListPools)
		api.POST("/admin/pools", middleware.AdminGuard(), s.handleAdminCreatePool)
		api.GET("/admin/lends", middleware.AdminGuard(), s.handleAdminListLends)
		api.GET("/admin/borrows", middleware.AdminGuard(), s.handleAdminListBorrows)
	}
}

func (s *Service) handleListPools(c *gin.Context) {
	pools, err := s.store.ListPools(PoolActive)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	var out []map[string]interface{}
	for _, p := range pools {
		out = append(out, map[string]interface{}{
			"id":             p.ID,
			"asset":          p.Asset,
			"total_supply":   p.TotalSupply.HumanString(),
			"total_borrow":   p.TotalBorrow.HumanString(),
			"available":      p.Available.HumanString(),
			"interest_rate":  p.InterestRate,
			"collateral_req": p.CollateralReq,
		})
	}
	response.JSON(c, gin.H{"pools": out})
}

func (s *Service) handlePoolInfo(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 4001, "invalid id")
		return
	}
	info, err := s.PoolInfo(id)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, info)
}

type lendReq struct {
	PoolID int64  `json:"pool_id"`
	Amount string `json:"amount"` // 人类可读金额
}

func (s *Service) handleLend(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req lendReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	amount, err := settlement.AssetAmountFromString(req.Amount, 8)
	if err != nil {
		response.Error(c, 400, 4001, "invalid amount")
		return
	}
	order, err := s.Lend(uid, req.PoolID, amount)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, order)
}

type borrowReq struct {
	PoolID      int64  `json:"pool_id"`
	BorrowAmt   string `json:"borrow_amount"`
	Collateral  string `json:"collateral"`
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
	borrowAmt, err := settlement.AssetAmountFromString(req.BorrowAmt, 8)
	if err != nil {
		response.Error(c, 400, 4001, "invalid borrow amount")
		return
	}
	collateral, err := settlement.AssetAmountFromString(req.Collateral, 8)
	if err != nil {
		response.Error(c, 400, 4002, "invalid collateral")
		return
	}
	order, err := s.Borrow(uid, req.PoolID, borrowAmt, collateral)
	if err != nil {
		response.Error(c, 400, 4003, err.Error())
		return
	}
	response.JSON(c, order)
}

func (s *Service) handleRepay(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 4001, "invalid id")
		return
	}
	order, err := s.Repay(uid, id)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, order)
}

func (s *Service) handleWithdraw(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 4001, "invalid id")
		return
	}
	order, err := s.Withdraw(uid, id)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, order)
}

func (s *Service) handleMyLends(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.store.ListLendOrdersByUser(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"lends": list})
}

func (s *Service) handleMyBorrows(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.store.ListBorrowOrdersByUser(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"borrows": list})
}

func (s *Service) handleAdminCreatePool(c *gin.Context) {
	var req struct {
		Asset         string  `json:"asset"`
		CollateralReq float64 `json:"collateral_req"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, err.Error())
		return
	}
	p, err := s.CreatePool(req.Asset, req.CollateralReq)
	if err != nil {
		response.Error(c, 400, 4001, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"pool": map[string]interface{}{
			"id":             p.ID,
			"asset":          p.Asset,
			"total_supply":   p.TotalSupply.HumanString(),
			"total_borrow":   p.TotalBorrow.HumanString(),
			"available":      p.Available.HumanString(),
			"interest_rate":  p.InterestRate,
			"collateral_req": p.CollateralReq,
			"status":         string(p.Status),
			"created_at":     p.CreatedAt,
		},
	})
}

func (s *Service) handleAdminListPools(c *gin.Context) {
	pools, err := s.store.ListPools("")
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	var out []map[string]interface{}
	for _, p := range pools {
		out = append(out, map[string]interface{}{
			"id":             p.ID,
			"asset":          p.Asset,
			"total_supply":   p.TotalSupply.HumanString(),
			"total_borrow":   p.TotalBorrow.HumanString(),
			"available":      p.Available.HumanString(),
			"interest_rate":  p.InterestRate,
			"collateral_req": p.CollateralReq,
			"status":         string(p.Status),
			"created_at":     p.CreatedAt,
		})
	}
	response.JSON(c, gin.H{"pools": out})
}

func (s *Service) handleAdminListLends(c *gin.Context) {
	list, err := s.store.ListAllLendOrders()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	var out []map[string]interface{}
	for _, o := range list {
		out = append(out, map[string]interface{}{
			"id":         o.ID,
			"user_id":    o.UserID,
			"pool_id":    o.PoolID,
			"amount":     o.Amount.HumanString(),
			"rate":       o.Rate,
			"status":     o.Status,
			"created_at": o.CreatedAt,
		})
	}
	response.JSON(c, gin.H{"lends": out})
}

func (s *Service) handleAdminListBorrows(c *gin.Context) {
	list, err := s.store.ListAllBorrowOrders()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	var out []map[string]interface{}
	for _, o := range list {
		out = append(out, map[string]interface{}{
			"id":           o.ID,
			"user_id":      o.UserID,
			"pool_id":      o.PoolID,
			"amount":       o.Amount.HumanString(),
			"collateral":   o.Collateral.HumanString(),
			"rate":         o.Rate,
			"interest_acc": o.InterestAcc.HumanString(),
			"status":       o.Status,
			"created_at":   o.CreatedAt,
			"repaid_at":    o.RepaidAt,
		})
	}
	response.JSON(c, gin.H{"borrows": out})
}
