package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 本文件是管理后台「期货交易管理」模块，代理 internal/futuresapi 的持仓/资金费/钱包管理端点
// （futuresapi 这些端点本身已带 AdminGuard，本层转发 admin 自签 token 即可；并按细粒度 RBAC
// 守卫：读需 futures:view，写需 futures:manage）。补全此前 adminapi 未暴露的期货交易管理能力
// （持仓、资金费、手工充值、代客直提、应急冻结/解冻、风控开关、社会化坏账分摊）。
//
// 上游 futures 服务返回统一信封 {code,data,message}；本层把 data 原样透传给前端，保持与直接
// 调用 futures 服务一致的响应结构。上游不可达/业务错误统一以 502 返回，避免伪造管理结果。

// proxyFuturesGet 透传 GET 到 futures 服务（转发 query，便于 user_id/symbol 等过滤），data 原样回传。
func (s *Server) proxyFuturesGet(c *gin.Context, path string) {
	base := s.serviceURL("futures")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "futures service not configured")
		return
	}
	full := path
	if q := c.Request.URL.RawQuery; q != "" {
		full = path + "?" + q
	}
	var raw json.RawMessage
	if err := s.up.Get(c.Request.Context(), base, full, &raw); err != nil {
		s.fail(c, http.StatusBadGateway, "futures service error: "+err.Error())
		return
	}
	s.ok(c, raw)
}

// proxyFuturesPost 透传 POST（携带请求体）到 futures 服务，data 原样回传。
func (s *Server) proxyFuturesPost(c *gin.Context, path string) {
	base := s.serviceURL("futures")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "futures service not configured")
		return
	}
	var body interface{}
	if c.Request.Body != nil {
		_ = json.NewDecoder(c.Request.Body).Decode(&body)
	}
	var raw json.RawMessage
	if err := s.up.Post(c.Request.Context(), base, path, &raw, body); err != nil {
		s.fail(c, http.StatusBadGateway, "futures service error: "+err.Error())
		return
	}
	s.ok(c, raw)
}

// --- 路由处理（路径前缀 /api/admin/futures，与 server.go 注册一致）---

// 持仓列表（支持 ?user_id=&symbol= 过滤，由前端透传）。
func (s *Server) handleFuturesPositions(c *gin.Context) {
	s.proxyFuturesGet(c, "/api/v1/futures/positions")
}

// 当前资金费率。
func (s *Server) handleFuturesFunding(c *gin.Context) {
	s.proxyFuturesGet(c, "/api/v1/futures/funding")
}

// 资金费率历史。
func (s *Server) handleFuturesFundingHistory(c *gin.Context) {
	s.proxyFuturesGet(c, "/api/v1/futures/funding-history")
}

// 手工入账（运营为某用户信用期货账户，高危）。
func (s *Server) handleFuturesDeposit(c *gin.Context) {
	s.proxyFuturesPost(c, "/api/v1/futures/wallet/deposit")
}

// 管理员代客直提（绕过冷静期直接链上广播，高危；已接风控引擎 RISK-F3）。
func (s *Server) handleFuturesWithdrawChain(c *gin.Context) {
	s.proxyFuturesPost(c, "/api/v1/futures/wallet/withdraw/chain")
}

// 紧急冻结出金（全部用户出金暂停，高危）。
func (s *Server) handleFuturesEmergencyFreeze(c *gin.Context) {
	s.proxyFuturesPost(c, "/api/v1/futures/wallet/withdraw/emergency/freeze")
}

// 解冻出金。
func (s *Server) handleFuturesEmergencyResume(c *gin.Context) {
	s.proxyFuturesPost(c, "/api/v1/futures/wallet/withdraw/emergency/resume")
}

// 风控开关（启用/停用期货风控引擎，高危）。
func (s *Server) handleFuturesRiskEnable(c *gin.Context) {
	s.proxyFuturesPost(c, "/api/v1/futures/wallet/risk/enable")
}

// 发起社会化坏账分摊提案。
func (s *Server) handleFuturesSocializePropose(c *gin.Context) {
	s.proxyFuturesPost(c, "/api/v1/futures/wallet/baddebt/socialize/propose")
}

// 审批社会化坏账分摊。
func (s *Server) handleFuturesSocializeApprove(c *gin.Context) {
	s.proxyFuturesPost(c, "/api/v1/futures/wallet/baddebt/socialize/approve")
}
