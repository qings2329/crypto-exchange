package earn

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"go.uber.org/zap"
)

// newTestServer 起一个真实 gin 引擎并注册 earn/launchpad 路由，返回引擎与签发 token 的 verifier。
func newTestServer() (*gin.Engine, *middleware.TokenVerifier, *Service) {
	gin.SetMode(gin.TestMode)
	l := ledger.New()
	for _, uid := range []int64{1, 2} {
		_ = l.ReceiveOnChain(uid, "USDT", amt(100000, 6), "seed")
	}
	svc := NewService(NewMemStore(), l, Config{}, zap.NewNop())
	base := time.Now()
	_ = svc.CreateProject(&LaunchProject{
		Name: "NEW 挖矿", Token: "NEW",
		StartsAt: base.Add(-time.Hour), EndsAt: base.AddDate(1, 0, 0),
		Pools: []LaunchPool{{ID: "usdt", Asset: "USDT", APY: 0.15}},
	})
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	svc.RegisterRoutes(r, verifier)
	return r, verifier, svc
}

// TestAdminEndpointsRequireAdmin F4：产品发行/项目创建/预算充值/对账均为管理员操作，
// 非管理员须 403；管理员放行到 handler（业务结果取决于数据，不得为 403）。
func TestEarnAdminEndpointsRequireAdmin(t *testing.T) {
	r, verifier, svc := newTestServer()
	adminTok := verifier.IssueRole(99, middleware.RoleAdmin, time.Hour)
	userTok := verifier.IssueRole(7, "user", time.Hour)

	projects, _ := svc.ListProjects()
	pid := projects[0].ID

	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/earn/products", `{"name":"p","asset":"USDT","apy":0.03,"min_amount":10}`},
		{http.MethodPost, "/api/v1/earn/admin/accrue", ""},
		{http.MethodPost, "/api/v1/launchpad/admin/projects",
			`{"name":"x","token":"NEW","starts_at":"2026-01-01T00:00:00Z","ends_at":"2026-02-01T00:00:00Z","pools":[{"id":"usdt","asset":"USDT","apy":0.1}]}`},
		{http.MethodPost, "/api/v1/launchpad/admin/fund", `{"project_id":` + strconv.FormatInt(pid, 10) + `,"amount":100}`},
		{http.MethodGet, "/api/v1/launchpad/admin/reconcile", ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+userTok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s: non-admin should be 403, got %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		req2 := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req2.Header.Set("Authorization", "Bearer "+adminTok)
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, req2)
		if w2.Code == http.StatusForbidden {
			t.Fatalf("%s %s: admin should pass guard, got 403 %s", tc.method, tc.path, w2.Body.String())
		}
	}
}

// TestSubscribeRequiresAgreement 契约：未勾选风险揭示（agreed=false）必须被拒。
func TestSubscribeRequiresAgreement(t *testing.T) {
	r, verifier, svc := newTestServer()
	if err := svc.CreateProduct(&EarnProduct{Name: "p", Asset: "USDT", APY: 0.03, MinAmount: 10}); err != nil {
		t.Fatal(err)
	}
	userTok := verifier.IssueRole(7, "user", time.Hour)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/earn/subscribe",
		strings.NewReader(`{"product_id":1,"amount":100,"agreed":false}`))
	req.Header.Set("Authorization", "Bearer "+userTok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expect 400 when agreement missing, got %d %s", w.Code, w.Body.String())
	}
}
