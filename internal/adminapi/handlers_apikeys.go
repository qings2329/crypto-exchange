package adminapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/apikeys"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// listApiKeys 列出 API Key（admin 视角）。可按 user_id 过滤；结果分页。
// GET /api/admin/apikeys?user_id=&limit=&offset=
func (s *Server) listApiKeys(c *gin.Context) {
	var filter apikeys.ListFilter
	if v := c.Query("user_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			filter.UserID = id
		}
	}
	keys, err := s.apiKeyStore.List(filter)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list failed")
		return
	}
	limit, offset := parsePage(c)
	page, total := paginate(keys, limit, offset)
	views := make([]apikeys.APIKeyView, 0, len(page))
	for _, k := range page {
		views = append(views, k.View())
	}
	s.ok(c, gin.H{"items": views, "total": total})
}

// getApiKey 返回单条 API Key 详情。GET /api/admin/apikeys/:id
func (s *Server) getApiKey(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	k, err := s.apiKeyStore.GetByID(id)
	if err != nil {
		if errors.Is(err, apikeys.ErrKeyNotFound) {
			s.fail(c, http.StatusNotFound, "api key not found")
			return
		}
		s.fail(c, http.StatusInternalServerError, "get failed")
		return
	}
	s.ok(c, k.View())
}

// createApiKey 为某用户签发一条新的 API Key（明文仅一次性返回）。
// POST /api/admin/apikeys  body: {user_id, label, permissions?}
func (s *Server) createApiKey(c *gin.Context) {
	var req struct {
		UserID      int64    `json:"user_id"`
		Label       string   `json:"label"`
		Permissions []string `json:"permissions"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == 0 || req.Label == "" {
		s.fail(c, http.StatusBadRequest, "user_id and label required")
		return
	}
	kp, err := apikeys.GenerateKey()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "generate key failed")
		return
	}
	createdBy, _ := middleware.UserID(c)
	record := &apikeys.APIKey{
		UserID:      req.UserID,
		Label:       req.Label,
		Prefix:      kp.Prefix,
		KeyHash:     apikeys.HashKey(kp.Key),
		Permissions: req.Permissions,
		CreatedBy:   createdBy,
	}
	if err := s.apiKeyStore.Create(record); err != nil {
		s.fail(c, http.StatusInternalServerError, "create failed")
		return
	}
	s.ok(c, gin.H{
		"key":     kp.Key, // 明文，仅此一次
		"api_key": record.View(),
	})
}

// revokeApiKey 吊销指定 API Key。DELETE /api/admin/apikeys/:id
func (s *Server) revokeApiKey(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.apiKeyStore.Revoke(id); err != nil {
		if errors.Is(err, apikeys.ErrKeyNotFound) {
			s.fail(c, http.StatusNotFound, "api key not found")
			return
		}
		if errors.Is(err, apikeys.ErrKeyRevoked) {
			s.fail(c, http.StatusConflict, "api key already revoked")
			return
		}
		s.fail(c, http.StatusInternalServerError, "revoke failed")
		return
	}
	s.ok(c, gin.H{"revoked": true, "id": id})
}
