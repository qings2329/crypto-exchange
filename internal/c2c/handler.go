package c2c

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// Handler 暴露 C2C 用户侧 HTTP 接口（下单/我的订单）以及管理侧聚合接口。
type Handler struct {
	svc      *Service
	verifier *middleware.TokenVerifier
}

// NewHandler 构造 C2C HTTP handler。
func NewHandler(svc *Service, verifier *middleware.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// Register 注册用户侧路由（需登录）。
func (h *Handler) Register(r *gin.Engine) {
	g := r.Group("/api/v1/c2c")
	g.Use(middleware.Auth(h.verifier))
	g.POST("/orders", h.create)
	g.GET("/orders", h.myOrders)
}

// create 用户发布一笔 C2C 挂单。
func (h *Handler) create(c *gin.Context) {
	var req struct {
		Side   string  `json:"side"`
		Coin   string  `json:"coin"`
		Amount float64 `json:"amount"`
		Price  float64 `json:"price"`
		Note   string  `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "bad request: "+err.Error())
		return
	}
	uid := loginUID(c)
	o, err := h.svc.Create(uid, Side(req.Side), req.Coin, req.Amount, req.Price, req.Note)
	if err != nil {
		response.Error(c, http.StatusBadRequest, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{"order": o})
}

// myOrders 分页查询当前用户的 C2C 挂单。
func (h *Handler) myOrders(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	items, total, err := h.svc.List(OrderFilter{UserID: loginUID(c)}, limit, offset)
	if err != nil {
		response.Error(c, http.StatusBadRequest, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{"items": items, "total": total})
}

// RegisterAdmin 注册管理侧聚合接口（供 cmd/admin 的 admin token 代理）。
// 此处与 user 服务的 adminG 一致，再叠加 AdminGuard 纵深防御。
func (h *Handler) RegisterAdmin(r *gin.Engine) {
	g := r.Group("/api/v1/c2c")
	g.Use(middleware.Auth(h.verifier), middleware.AdminGuard())
	g.GET("/orders", h.myOrders)
	g.POST("/orders/:id/:action", h.adminAction)
}

// adminAction 管理员对订单执行动作（冻结/解冻/完成）。
func (h *Handler) adminAction(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, http.StatusBadRequest, 400, "bad order id")
		return
	}
	var o *Order
	switch c.Param("action") {
	case "freeze":
		o, err = h.svc.Freeze(id)
	case "release":
		o, err = h.svc.Release(id)
	case "complete":
		o, err = h.svc.Complete(id)
	default:
		response.Error(c, http.StatusBadRequest, 400, "unknown action")
		return
	}
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, ErrBadTransition) {
			code = http.StatusConflict
		}
		response.Error(c, code, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{"order": o})
}

// loginUID 从请求上下文读取登录用户 ID（由 middleware.Auth 注入）。
func loginUID(c *gin.Context) int64 {
	if uid, ok := middleware.UserID(c); ok {
		return uid
	}
	return 0
}
