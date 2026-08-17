package announcement

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// Handler 暴露公告 HTTP 接口。公开列表免鉴权；管理后台增删改需 admin 角色。
type Handler struct {
	svc *Service
}

// NewHandler 构造 handler。
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Register 路由。公开接口挂在 /api/v1/announcement，管理接口挂在 /api/v1/announcement/admin。
func (h *Handler) Register(r *gin.Engine, verifier *middleware.TokenVerifier) {
	g := r.Group("/api/v1/announcement")
	// 公开：已发布公告列表（首页/公告页展示）。
	g.GET("/list", h.listActive)

	// 管理后台聚合接口：需 admin 角色。
	adm := g.Group("/admin")
	adm.Use(middleware.Auth(verifier), middleware.AdminGuard())
	adm.GET("", h.adminList)
	adm.POST("", h.adminCreate)
	adm.PUT("/:id", h.adminUpdate)
	adm.DELETE("/:id", h.adminDelete)
}

func (h *Handler) listActive(c *gin.Context) {
	list, err := h.svc.ListActive()
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"announcements": list})
}

// RegisterAdminRoutes 在已鉴权分组（Auth+AdminGuard 由调用方套好）下注册公告管理接口，
// 复用同一套 handler 与错误映射。用于独立管理后端（cmd/admin）接入公告管理：
// 当 g 为 /api/admin 分组时，路径为 /api/admin/announcements
// （GET 列表 / POST 新建 / PUT :id 更新 / DELETE :id 删除）。
func (h *Handler) RegisterAdminRoutes(g *gin.RouterGroup) {
	g.GET("/announcements", h.adminList)
	g.POST("/announcements", h.adminCreate)
	g.PUT("/announcements/:id", h.adminUpdate)
	g.DELETE("/announcements/:id", h.adminDelete)
}

func (h *Handler) adminList(c *gin.Context) {
	list, err := h.svc.ListAll()
	if err != nil {
		fail(c, err)
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	total := len(list)
	page := list
	if total > 0 {
		if offset >= total {
			page = list[:0]
		} else {
			end := offset + limit
			if end > total {
				end = total
			}
			page = list[offset:end]
		}
	}
	response.JSON(c, gin.H{"announcements": page, "total": total})
}

func (h *Handler) adminCreate(c *gin.Context) {
	var req struct {
		Level   *string `json:"level"`
		Title   *string `json:"title"`
		Content *string `json:"content"`
		Active  *bool   `json:"active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	a, err := h.svc.Create(AnnouncementInput{
		Level:   req.Level,
		Title:   req.Title,
		Content: req.Content,
		Active:  req.Active,
	})
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, a)
}

func (h *Handler) adminUpdate(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid id")
		return
	}
	var req struct {
		Level   *string `json:"level"`
		Title   *string `json:"title"`
		Content *string `json:"content"`
		Active  *bool   `json:"active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	a, err := h.svc.Update(id, AnnouncementInput{
		Level:   req.Level,
		Title:   req.Title,
		Content: req.Content,
		Active:  req.Active,
	})
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, a)
}

func (h *Handler) adminDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid id")
		return
	}
	if err := h.svc.Delete(id); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

// fail 把业务错误映射为 HTTP 响应。
func fail(c *gin.Context, err error) {
	switch err {
	case ErrNotFound:
		response.Error(c, http.StatusNotFound, 404, err.Error())
	case ErrInvalidLevel, ErrTitleRequired, ErrTitleTooLong, ErrContentTooLong:
		response.Error(c, http.StatusBadRequest, 400, err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, 500, err.Error())
	}
}
