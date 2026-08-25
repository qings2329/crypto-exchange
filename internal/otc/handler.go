package otc

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// RegisterRoutes 在 gin 引擎上注册场外交易路由。
// 普通业务路由受 middleware.Auth(verifier) 保护（需合法 HMAC Bearer Token）；
// 争议裁决 /admin/orders /admin/reconcile 另加 middleware.AdminGuard()（仅管理员可调用，见 F4）。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/otc")
	api.Use(middleware.Auth(verifier))
	{
		api.POST("/advertisements", s.handleCreateAd)
		api.GET("/advertisements", s.handleListAds)
		api.GET("/prices", s.handlePrices)
		api.POST("/orders/take", s.handleTakeOrder)
		api.POST("/orders/:id/pay", s.handleMarkPaid)
		api.POST("/orders/:id/complete", s.handleConfirmComplete)
		api.POST("/orders/:id/cancel", s.handleCancelOrder)
		api.POST("/orders/:id/dispute", s.handleOpenDispute)
		api.POST("/orders/:id/resolve", middleware.AdminGuard(), s.handleResolveDispute)
		api.GET("/orders", s.handleListOrders)
		api.GET("/admin/orders", middleware.AdminGuard(), s.handleAdminOrders)
		api.GET("/counterparties", s.handleListCounterparties)
		api.GET("/admin/reconcile", middleware.AdminGuard(), s.handleReconcile)
		// 订单沟通与付款凭证（订单参与方可见）
		api.GET("/orders/:id/messages", s.handleListMessages)
		api.POST("/orders/:id/messages", s.handleSendMessage)
		api.GET("/orders/:id/proofs", s.handleListProofs)
		api.POST("/orders/:id/proofs", s.handleUploadProof)
		api.GET("/orders/:id/proofs/:file", s.handleGetProof)
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

// handlePrices GET /api/v1/otc/prices?asset=&fiat= → 法币报价（前端 OtcPrice 契约）。
func (s *Service) handlePrices(c *gin.Context) {
	q, err := s.FiatQuote(c.Query("asset"), c.Query("fiat"))
	if err != nil {
		response.Error(c, http.StatusNotFound, 404, err.Error())
		return
	}
	response.JSON(c, q)
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
	escrows, stuck, err := s.Reconcile()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	// F5b：用 AssetAmount.IsZero 判断余额是否清零，不引入 1e-9 浮点容差。
	balanced := true
	eb := make(map[string]float64, len(escrows))
	for a, bal := range escrows {
		eb[a] = bal.HumanFloat()
		if !bal.IsZero() {
			balanced = false
		}
	}
	response.JSON(c, gin.H{
		"escrow_balance": eb,
		"stuck_orders":   stuck,
		"balanced":       balanced,
	})
}

func parseOrderID(c *gin.Context) (int64, error) {
	return strconv.ParseInt(c.Param("id"), 10, 64)
}

// writeOrderErr 将订单相关错误映射为 HTTP 状态：非参与方 403，订单不存在 404，其余 400。
func writeOrderErr(c *gin.Context, err error) {
	switch err {
	case ErrNotParty:
		response.Error(c, 403, 4030, err.Error())
	case ErrOrderNotFound:
		response.Error(c, 404, 4040, err.Error())
	default:
		response.Error(c, 400, 4002, err.Error())
	}
}

// ---- 订单沟通 / 付款凭证 ----

func (s *Service) handleListMessages(c *gin.Context) {
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
	list, err := s.ListMessages(id, uid)
	if err != nil {
		writeOrderErr(c, err)
		return
	}
	response.JSON(c, gin.H{"messages": list})
}

func (s *Service) handleSendMessage(c *gin.Context) {
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
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	m, err := s.SendMessage(id, uid, req.Content)
	if err != nil {
		writeOrderErr(c, err)
		return
	}
	response.JSON(c, m)
}

func (s *Service) handleListProofs(c *gin.Context) {
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
	list, err := s.ListProofs(id, uid)
	if err != nil {
		writeOrderErr(c, err)
		return
	}
	response.JSON(c, gin.H{"proofs": list})
}

func (s *Service) handleUploadProof(c *gin.Context) {
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
	file, err := c.FormFile("file")
	if err != nil {
		response.Error(c, 400, 4000, "file required")
		return
	}
	f, err := file.Open()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	p, err := s.UploadProof(id, uid, file.Filename, file.Header.Get("Content-Type"), data)
	if err != nil {
		writeOrderErr(c, err)
		return
	}
	response.JSON(c, p)
}

// handleGetProof 提供已上传凭证文件的下载（仅订单参与方可访问）。
func (s *Service) handleGetProof(c *gin.Context) {
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
	if _, err := s.orderPartyGuard(id, uid); err != nil {
		writeOrderErr(c, err)
		return
	}
	file := filepath.Base(c.Param("file")) // 防目录穿越：仅取 basename
	if file == "" || file == "." || file == string(os.PathSeparator) {
		response.Error(c, 400, 4000, "invalid file")
		return
	}
	full := filepath.Join(s.uploadDir, file)
	// 二次兜底：确保解析结果仍在 uploadDir 内。
	if !strings.HasPrefix(full, s.uploadDir) {
		response.Error(c, 400, 4000, "invalid file")
		return
	}
	c.File(full)
}
