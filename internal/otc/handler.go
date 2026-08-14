package otc

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// RegisterRoutes 在 gin 引擎上注册场外交易路由。
// 业务路由受 middleware.Auth(verifier) 保护（需合法 HMAC Bearer Token）。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/otc")
	api.Use(middleware.Auth(verifier))
	{
		api.POST("/advertisements", s.handleCreateAd)
		api.GET("/advertisements", s.handleListAds)
		api.POST("/orders/take", s.handleTakeOrder)
		api.POST("/orders/:id/pay", s.handleMarkPaid)
		api.POST("/orders/:id/complete", s.handleConfirmComplete)
		api.POST("/orders/:id/cancel", s.handleCancelOrder)
		api.POST("/orders/:id/dispute", s.handleOpenDispute)
		api.POST("/orders/:id/resolve", s.handleResolveDispute)
		api.GET("/orders", s.handleListOrders)
		api.GET("/admin/orders", s.handleAdminOrders)
		api.GET("/counterparties", s.handleListCounterparties)
		api.GET("/admin/reconcile", s.handleReconcile)
	}
}

type createAdReq struct {
	Side           string  `json:"side"`
	Asset          string  `json:"asset"`
	FiatCurrency   string  `json:"fiat_currency"`
	Price          float64 `json:"price"`
	MinAmount      float64 `json:"min_amount"`
	MaxAmount      float64 `json:"max_amount"`
	PaymentMethods string  `json:"payment_methods"`
}

func (s *Service) handleCreateAd(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req createAdReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.Side != "buy" && req.Side != "sell" {
		response.Error(c, 400, 4001, "side must be buy or sell")
		return
	}
	if req.Price <= 0 || req.MinAmount <= 0 || req.MaxAmount < req.MinAmount {
		response.Error(c, 400, 4001, "price/min_amount/max_amount required and valid")
		return
	}
	ad := &OtcAdvertisement{
		UserID:         uid,
		Side:           AdSide(req.Side),
		Asset:          req.Asset,
		FiatCurrency:   req.FiatCurrency,
		Price:          req.Price,
		MinAmount:      req.MinAmount,
		MaxAmount:      req.MaxAmount,
		PaymentMethods: req.PaymentMethods,
	}
	if err := s.CreateAdvertisement(ad); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, ad)
}

func (s *Service) handleListAds(c *gin.Context) {
	side := AdSide(c.Query("side"))
	asset := c.Query("asset")
	if side != SideBuy && side != SideSell {
		side = ""
	}
	list, err := s.ListAdvertisements(side, asset)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"advertisements": list})
}

type takeOrderReq struct {
	AdID           int64   `json:"ad_id"`
	FiatAmount     float64 `json:"fiat_amount"`
	PaymentMethod  string  `json:"payment_method"`
}

func (s *Service) handleTakeOrder(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req takeOrderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.AdID <= 0 || req.FiatAmount <= 0 {
		response.Error(c, 400, 4001, "ad_id/fiat_amount required")
		return
	}
	o, err := s.TakeOrder(req.AdID, uid, req.FiatAmount, req.PaymentMethod)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, o)
}

func (s *Service) handleMarkPaid(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	id, err := parseOrderID(c)
	if err != nil {
		response.Error(c, 400, 4000, "invalid order id")
		return
	}
	if err := s.MarkPaid(id, uid); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

type completeReq struct {
	Rating int `json:"rating"`
}

func (s *Service) handleConfirmComplete(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	id, err := parseOrderID(c)
	if err != nil {
		response.Error(c, 400, 4000, "invalid order id")
		return
	}
	var req completeReq
	_ = c.ShouldBindJSON(&req)
	if err := s.ConfirmComplete(id, uid, req.Rating); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (s *Service) handleCancelOrder(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	id, err := parseOrderID(c)
	if err != nil {
		response.Error(c, 400, 4000, "invalid order id")
		return
	}
	if err := s.CancelOrder(id, uid); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (s *Service) handleOpenDispute(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	id, err := parseOrderID(c)
	if err != nil {
		response.Error(c, 400, 4000, "invalid order id")
		return
	}
	if err := s.OpenDispute(id, uid); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

type resolveReq struct {
	RefundToSeller bool `json:"refund_to_seller"`
	Rating         int  `json:"rating"`
}

func (s *Service) handleResolveDispute(c *gin.Context) {
	id, err := parseOrderID(c)
	if err != nil {
		response.Error(c, 400, 4000, "invalid order id")
		return
	}
	var req resolveReq
	_ = c.ShouldBindJSON(&req)
	if err := s.ResolveDispute(id, req.RefundToSeller, req.Rating); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (s *Service) handleListOrders(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.ListOrders(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"orders": list})
}

func (s *Service) handleAdminOrders(c *gin.Context) {
	list, err := s.AdminListOrders()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"orders": list})
}

func (s *Service) handleListCounterparties(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.ListCounterparties(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"counterparties": list})
}

func (s *Service) handleReconcile(c *gin.Context) {
	escrow, stuck, err := s.Reconcile()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"escrow_balance": escrow,
		"stuck_orders":   stuck,
		"balanced":       escrow < 1e-9,
	})
}

func parseOrderID(c *gin.Context) (int64, error) {
	return strconv.ParseInt(c.Param("id"), 10, 64)
}
