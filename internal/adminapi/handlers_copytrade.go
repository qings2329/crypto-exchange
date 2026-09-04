package adminapi

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleAdminCopyLeads 代理 GET copytrade 服务 /api/v1/copytrade/admin/leads。
func (s *Server) handleAdminCopyLeads(c *gin.Context) {
	base := s.serviceURL("copytrade")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "copytrade service not configured")
		return
	}
	var out struct {
		Leads []map[string]interface{} `json:"leads"`
	}
	if err := s.up.Get(c.Request.Context(), base, "/api/v1/copytrade/admin/leads", &out); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("copytrade upstream: %v", err))
		return
	}
	s.ok(c, gin.H{"leads": out.Leads})
}

// handleAdminCopyFollows 代理 GET copytrade 服务 /api/v1/copytrade/admin/follows。
func (s *Server) handleAdminCopyFollows(c *gin.Context) {
	base := s.serviceURL("copytrade")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "copytrade service not configured")
		return
	}
	var out struct {
		Follows []map[string]interface{} `json:"follows"`
	}
	if err := s.up.Get(c.Request.Context(), base, "/api/v1/copytrade/admin/follows", &out); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("copytrade upstream: %v", err))
		return
	}
	s.ok(c, gin.H{"follows": out.Follows})
}

// handleAdminCopyCopies 代理 GET copytrade 服务 /api/v1/copytrade/admin/copies。
func (s *Server) handleAdminCopyCopies(c *gin.Context) {
	base := s.serviceURL("copytrade")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "copytrade service not configured")
		return
	}
	var out struct {
		Copies []map[string]interface{} `json:"copies"`
	}
	if err := s.up.Get(c.Request.Context(), base, "/api/v1/copytrade/admin/copies", &out); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("copytrade upstream: %v", err))
		return
	}
	s.ok(c, gin.H{"copies": out.Copies})
}

// handleAdminCopyReconcile 代理 GET copytrade 服务 /api/v1/copytrade/admin/reconcile。
func (s *Server) handleAdminCopyReconcile(c *gin.Context) {
	base := s.serviceURL("copytrade")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "copytrade service not configured")
		return
	}
	var out map[string]interface{}
	if err := s.up.Get(c.Request.Context(), base, "/api/v1/copytrade/admin/reconcile", &out); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("copytrade upstream: %v", err))
		return
	}
	s.ok(c, out)
}
