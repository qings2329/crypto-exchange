package wealth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"go.uber.org/zap"
)

// newTestServer 起一个真实 gin 引擎并注册 wealth 路由，返回引擎与签发 token 的 verifier。
func newTestServer() (*gin.Engine, *middleware.TokenVerifier, *Service) {
	gin.SetMode(gin.TestMode)
	store := NewMemStore()
	l := ledger.New()
	for _, uid := range []int64{1, 2} {
		_ = l.Deposit(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed")
	}
	svc := NewService(store, l, Config{}, zap.NewNop())
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	svc.RegisterRoutes(r, verifier)
	return r, verifier, svc
}

// TestAdminEndpointsRequireAdmin F4：发行产品/全量持仓/触发计息均为管理员操作，非管理员须 403。
func TestAdminEndpointsRequireAdmin(t *testing.T) {
	r, verifier, _ := newTestServer()
	adminTok := verifier.IssueRole(99, middleware.RoleAdmin, time.Hour)
	userTok := verifier.IssueRole(7, "user", time.Hour)

	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/wealth/products", `{"name":"p","asset":"USDT","type":"current","annual_rate":0.03}`},
		{http.MethodGet, "/api/v1/wealth/admin/holdings", ""},
		{http.MethodPost, "/api/v1/wealth/admin/accrue", ""},
	}
	for _, tc := range cases {
		// 非管理员：必须 403。
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+userTok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s: non-admin should be 403, got %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		// 管理员：通过护栏（到达 handler，业务结果取决于数据，不得为 403）。
		req2 := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req2.Header.Set("Authorization", "Bearer "+adminTok)
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, req2)
		if w2.Code == http.StatusForbidden {
			t.Fatalf("%s %s: admin should pass guard, got 403 %s", tc.method, tc.path, w2.Body.String())
		}
	}
}
