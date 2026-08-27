package referral

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// Handler 暴露佣金 HTTP 接口。
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 注册路由。
func (h *Handler) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	g := r.Group("/api/v1/referral")
	auth := g.Group("")
	auth.Use(middleware.Auth(verifier))
	auth.GET("/stats", h.stats)
	auth.GET("/commissions", h.listMyCommissions)
}

// GET /api/v1/referral/stats
func (h *Handler) stats(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	stats, err := h.svc.GetMyReferralStats(uid)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"totals": stats})
}

// GET /api/v1/referral/commissions?limit=20&offset=0
func (h *Handler) listMyCommissions(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	commissions, total, err := h.svc.ListByReferrer(uid, limit, offset)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"commissions": commissions, "total": total})
}
