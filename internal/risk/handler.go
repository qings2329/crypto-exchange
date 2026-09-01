package risk

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// Handler 是风控 HTTP 层，仅做参数绑定、调 Service、返回。
type Handler struct {
	svc *Service
}

// NewHandler 创建风控 handler。
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 注册路由。
// 全部为运营/管理向接口，一律要求管理员（F4）：本组原本只有全局 middleware.Auth，
// 意味着任意登录用户都能改风控规则（关掉全站提现限额）、删黑名单解封自己、
// 或读取全量 AML 黑名单与他人风控事件。
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	g := r.Group("/api/v1/risk")
	g.Use(middleware.AdminGuard())
	{
		g.POST("/rules", h.addRule)
		g.GET("/rules", h.listRules)
		g.POST("/blacklist", h.addBlacklist)
		g.DELETE("/blacklist", h.removeBlacklist)
		g.GET("/blacklist", h.listBlacklist)
		g.GET("/blacklist/check", h.checkBlacklist)
		g.POST("/check/withdraw", h.checkWithdraw)
		g.POST("/check/order", h.checkOrder)
		g.POST("/check/position", h.checkPosition)
		g.POST("/check/frequency", h.checkFrequency)
		g.GET("/events", h.listEvents)
	}
}

func (h *Handler) addRule(c *gin.Context) {
	var in RiskRule
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Error(c, 400, 400, "invalid body")
		return
	}
	r, err := h.svc.AddRule(&in)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	response.JSON(c, r)
}

func (h *Handler) listRules(c *gin.Context) {
	rules, err := h.svc.ListRules(c.Query("kind"))
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"items": rules, "count": len(rules)})
}

func (h *Handler) addBlacklist(c *gin.Context) {
	var in struct {
		Target string `json:"target"`
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Error(c, 400, 400, "invalid body")
		return
	}
	b, err := h.svc.AddBlacklist(in.Target, in.Kind, in.Reason)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	response.JSON(c, b)
}

func (h *Handler) removeBlacklist(c *gin.Context) {
	target := c.Query("target")
	if target == "" {
		response.Error(c, 400, 400, "target required")
		return
	}
	if err := h.svc.RemoveBlacklist(target); err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (h *Handler) listBlacklist(c *gin.Context) {
	items, err := h.svc.ListBlacklist(c.Query("kind"))
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"items": items, "count": len(items)})
}

func (h *Handler) checkBlacklist(c *gin.Context) {
	target := c.Query("target")
	if target == "" {
		response.Error(c, 400, 400, "target required")
		return
	}
	ok, err := h.svc.IsBlacklisted(target)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"target": target, "blacklisted": ok})
}

func (h *Handler) checkWithdraw(c *gin.Context) {
	var in struct {
		UserID   int64   `json:"user_id"`
		Asset    string  `json:"asset"`
		Amount   float64 `json:"amount"`
		KYCLevel int     `json:"kyc_level"`
		Address  string  `json:"address"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Error(c, 400, 400, "invalid body")
		return
	}
	res, err := h.svc.CheckWithdraw(in.UserID, in.Asset, in.Amount, in.KYCLevel, in.Address)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, res)
}

func (h *Handler) checkOrder(c *gin.Context) {
	var in struct {
		UserID   int64   `json:"user_id"`
		Asset    string  `json:"asset"`
		Qty      float64 `json:"qty"`
		KYCLevel int     `json:"kyc_level"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Error(c, 400, 400, "invalid body")
		return
	}
	res, err := h.svc.CheckOrder(in.UserID, in.Asset, in.Qty, in.KYCLevel)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, res)
}

func (h *Handler) checkPosition(c *gin.Context) {
	var in struct {
		UserID   int64   `json:"user_id"`
		Asset    string  `json:"asset"`
		Size     float64 `json:"size"`
		KYCLevel int     `json:"kyc_level"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Error(c, 400, 400, "invalid body")
		return
	}
	res, err := h.svc.CheckPosition(in.UserID, in.Asset, in.Size, in.KYCLevel)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, res)
}

func (h *Handler) checkFrequency(c *gin.Context) {
	var in struct {
		UserID int64  `json:"user_id"`
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Error(c, 400, 400, "invalid body")
		return
	}
	res, err := h.svc.CheckFrequency(in.UserID, in.Action, 24*time.Hour)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, res)
}

func (h *Handler) listEvents(c *gin.Context) {
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := h.svc.ListEvents(userID, limit)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"items": items, "count": len(items)})
}
