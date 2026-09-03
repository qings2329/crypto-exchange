package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 本文件是管理后台「风控管理」模块，代理 internal/risk 服务的规则/黑名单增删查接口
// （风控引擎的强制网关，运营可在后台直接配置限额/黑名单，无需直连 risk 服务或调裸 API）。
// 全部经 UpstreamClient 转发，复用 admin 自签的 role=admin token（上游 middleware.Auth 仅校验
// 签名+过期），并按细粒度 RBAC 守卫：读需 risk:view，增删需 risk:manage。
//
// 上游 risk 服务返回统一信封 {code,data,message}；本层把 data 部分原样透传给前端，保持与
// 直接调用 risk 服务一致的响应结构。上游不可达/业务错误统一以 502 返回，避免向运营展示
// 伪造的风控配置。

// riskBase 返回 risk 上游基址；未配置则返回空串（调用方据此返回 502）。
func (s *Server) riskBase() string { return s.serviceURL("risk") }

// proxyRiskGet 透传 GET 到 risk 服务，并把 data 原样回传。
func (s *Server) proxyRiskGet(c *gin.Context, path string) {
	base := s.riskBase()
	if base == "" {
		s.fail(c, http.StatusBadGateway, "risk service not configured")
		return
	}
	var raw json.RawMessage
	if err := s.up.Get(c.Request.Context(), base, path, &raw); err != nil {
		s.fail(c, http.StatusBadGateway, "risk service error: "+err.Error())
		return
	}
	s.ok(c, raw)
}

// proxyRiskPost 透传 POST（携带请求体）到 risk 服务，并把 data 原样回传。
func (s *Server) proxyRiskPost(c *gin.Context, path string) {
	base := s.riskBase()
	if base == "" {
		s.fail(c, http.StatusBadGateway, "risk service not configured")
		return
	}
	var body interface{}
	if c.Request.Body != nil {
		_ = json.NewDecoder(c.Request.Body).Decode(&body)
	}
	var raw json.RawMessage
	if err := s.up.Post(c.Request.Context(), base, path, &raw, body); err != nil {
		s.fail(c, http.StatusBadGateway, "risk service error: "+err.Error())
		return
	}
	s.ok(c, raw)
}

// proxyRiskDelete 透传 DELETE（带 query）到 risk 服务，并把 data 原样回传。
func (s *Server) proxyRiskDelete(c *gin.Context, path string) {
	base := s.riskBase()
	if base == "" {
		s.fail(c, http.StatusBadGateway, "risk service not configured")
		return
	}
	var raw json.RawMessage
	if err := s.up.Delete(c.Request.Context(), base, path, &raw); err != nil {
		s.fail(c, http.StatusBadGateway, "risk service error: "+err.Error())
		return
	}
	s.ok(c, raw)
}

// --- 路由处理（路径前缀 /api/admin/risk，与 server.go 注册一致）---

func (s *Server) handleRiskRulesList(c *gin.Context) {
	s.proxyRiskGet(c, "/api/v1/risk/rules")
}

func (s *Server) handleRiskRuleCreate(c *gin.Context) {
	s.proxyRiskPost(c, "/api/v1/risk/rules")
}

func (s *Server) handleRiskBlacklistList(c *gin.Context) {
	s.proxyRiskGet(c, "/api/v1/risk/blacklist?kind="+c.Query("kind"))
}

func (s *Server) handleRiskBlacklistCreate(c *gin.Context) {
	s.proxyRiskPost(c, "/api/v1/risk/blacklist")
}

func (s *Server) handleRiskBlacklistDelete(c *gin.Context) {
	target := c.Query("target")
	if target == "" {
		s.fail(c, http.StatusBadRequest, "target required")
		return
	}
	s.proxyRiskDelete(c, "/api/v1/risk/blacklist?target="+target)
}

func (s *Server) handleRiskBlacklistCheck(c *gin.Context) {
	target := c.Query("target")
	if target == "" {
		s.fail(c, http.StatusBadRequest, "target required")
		return
	}
	s.proxyRiskGet(c, "/api/v1/risk/blacklist/check?target="+target)
}

// handleRiskCheckWithdraw 供后台「提现风控预检」：把目标用户/资产/金额/地址送给 risk 引擎
// 评估，返回是否放行及原因，便于运营在审批前先行核对（只读，不落库）。
func (s *Server) handleRiskCheckWithdraw(c *gin.Context) {
	s.proxyRiskPost(c, "/api/v1/risk/check/withdraw")
}

// handleRiskEventList 代理 risk 引擎的触发事件列表（真实风控告警源），供风控大盘实时流使用。
func (s *Server) handleRiskEventList(c *gin.Context) {
	qs := ""
	uid := c.Query("user_id")
	limit := c.Query("limit")
	if uid != "" {
		qs += "user_id=" + uid + "&"
	}
	if limit != "" {
		qs += "limit=" + limit + "&"
	}
	s.proxyRiskGet(c, "/api/v1/risk/events?"+qs)
}
