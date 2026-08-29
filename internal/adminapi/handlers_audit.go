package adminapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// auditMiddleware 仅记录操作成功的变更类（非 GET/HEAD）管理操作到审计日志。
// 仅落元数据（方法/路由/状态码/IP/时间），不记录请求体，避免泄露口令等敏感信息。
func (s *Server) auditMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			return
		}
		// 仅记录成功操作（2xx 状态码）
		status := c.Writer.Status()
		if status < 200 || status >= 300 {
			return
		}
		uid, _ := middleware.UserID(c)
		action := ""
		switch c.Request.Method {
		case http.MethodPost:
			action = "create"
		case http.MethodPut, http.MethodPatch:
			action = "update"
		case http.MethodDelete:
			action = "delete"
		}
		_ = s.auditStore.Append(AuditEntry{
			AdminID: uid,
			Method:  c.Request.Method,
			Path:    c.FullPath(),
			Action:  action,
			Target:  c.Request.URL.Path,
			Status:  status,
			IP:      c.ClientIP(),
			Time:    time.Now().UnixNano(),
		})
	}
}

// handleAuditLogs 返回审计日志分页列表（按时间倒序）。需 audit:read 权限。
func (s *Server) handleAuditLogs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	logs, total, err := s.auditStore.List(limit, offset)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, 500, err.Error())
		return
	}
	s.ok(c, gin.H{"logs": logs, "total": total})
}
