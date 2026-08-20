package adminapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// handleAdminLendingPoolsCreate 代理 POST lending 服务 /api/v1/lending/admin/pools。
func (s *Server) handleAdminLendingPoolsCreate(c *gin.Context) {
	base := s.serviceURL("lending")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "lending service not configured")
		return
	}
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		s.fail(c, http.StatusBadRequest, err.Error())
		return
	}
	var out map[string]interface{}
	if err := s.up.Post(c.Request.Context(), base, "/api/v1/lending/admin/pools", &out, body); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("lending upstream: %v", err))
		return
	}
	s.ok(c, out)
}

// handleAdminLendingPools 代理 GET lending 服务 /api/v1/lending/admin/pools。
func (s *Server) handleAdminLendingPools(c *gin.Context) {
	base := s.serviceURL("lending")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "lending service not configured")
		return
	}
	var out struct {
		Pools []map[string]interface{} `json:"pools"`
	}
	if err := s.up.Get(c.Request.Context(), base, "/api/v1/lending/admin/pools", &out); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("lending upstream: %v", err))
		return
	}
	s.ok(c, gin.H{"pools": out.Pools})
}

// handleAdminLendingLends 代理 GET lending 服务 /api/v1/lending/admin/lends。
func (s *Server) handleAdminLendingLends(c *gin.Context) {
	base := s.serviceURL("lending")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "lending service not configured")
		return
	}
	var out struct {
		Lends []map[string]interface{} `json:"lends"`
	}
	if err := s.up.Get(c.Request.Context(), base, "/api/v1/lending/admin/lends", &out); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("lending upstream: %v", err))
		return
	}
	s.ok(c, gin.H{"lends": out.Lends})
}

// handleAdminLendingBorrows 代理 GET lending 服务 /api/v1/lending/admin/borrows。
func (s *Server) handleAdminLendingBorrows(c *gin.Context) {
	base := s.serviceURL("lending")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "lending service not configured")
		return
	}
	var out struct {
		Borrows []map[string]interface{} `json:"borrows"`
	}
	if err := s.up.Get(c.Request.Context(), base, "/api/v1/lending/admin/borrows", &out); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("lending upstream: %v", err))
		return
	}
	s.ok(c, gin.H{"borrows": out.Borrows})
}

// handleAdminBotStrategies 代理 GET bot 服务 /api/v1/bot/admin/strategies。
func (s *Server) handleAdminBotStrategies(c *gin.Context) {
	base := s.serviceURL("bot")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "bot service not configured")
		return
	}
	var out struct {
		Strategies []map[string]interface{} `json:"strategies"`
	}
	if err := s.up.Get(c.Request.Context(), base, "/api/v1/bot/admin/strategies", &out); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("bot upstream: %v", err))
		return
	}
	s.ok(c, gin.H{"strategies": out.Strategies})
}

// handleAdminBotTick 代理 POST bot 服务 /api/v1/bot/admin/strategies/:id/tick。
func (s *Server) handleAdminBotTick(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		s.fail(c, http.StatusBadRequest, "missing strategy id")
		return
	}
	base := s.serviceURL("bot")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "bot service not configured")
		return
	}
	var out map[string]interface{}
	path := "/api/v1/bot/admin/strategies/" + strings.TrimSpace(id) + "/tick"
	if err := s.up.Post(c.Request.Context(), base, path, &out, struct{}{}); err != nil {
		s.fail(c, http.StatusBadGateway, fmt.Sprintf("bot upstream: %v", err))
		return
	}
	s.ok(c, out)
}
