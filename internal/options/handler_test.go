package options

import (
	"fmt"
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

// newTestServer 起一个真实 gin 引擎并注册 options 路由，返回引擎与签发 token 的 verifier。
func newTestServer() (*gin.Engine, *middleware.TokenVerifier, *Service) {
	gin.SetMode(gin.TestMode)
	store := NewMemStore()
	l := ledger.New()
	for _, uid := range []int64{1, 2} {
		_ = l.Deposit(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed")
	}
	priceFn := func(asset string) (float64, bool) {
		if asset == "BTC" {
			return 50000, true
		}
		return 0, false
	}
	cfg := Config{QuoteAsset: "USDT", RiskFreeRate: 0.03, Volatility: 0.6, MarginRatio: 0.3, SettleInterval: time.Second}
	svc := NewService(store, l, cfg, zap.NewNop(), priceFn)
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	svc.RegisterRoutes(r, verifier)
	return r, verifier, svc
}

// TestAdminEndpointsRequireAdmin F4：结算/管理员持仓列表必须要求管理员角色，否则任意登录用户可越权操作。
func TestAdminEndpointsRequireAdmin(t *testing.T) {
	r, verifier, svc := newTestServer()
	// 准备一个已到期持仓供 /settle 使用。
	c := mustContract(svc, 1000, time.Now().Add(-time.Hour))
	p, _ := svc.OpenPosition(1, c.ID, SideLong, 1)

	adminTok := verifier.IssueRole(99, middleware.RoleAdmin, time.Hour)
	userTok := verifier.IssueRole(7, "user", time.Hour)

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/options/admin/positions", ""},
		{http.MethodPost, "/api/v1/options/settle", fmt.Sprintf(`{"position_id":%d}`, p.ID)},
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
		// 管理员：通过护栏（到达 handler，状态取决于业务，不得为 403）。
		req2 := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req2.Header.Set("Authorization", "Bearer "+adminTok)
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, req2)
		if w2.Code == http.StatusForbidden {
			t.Fatalf("%s %s: admin should pass guard, got 403 %s", tc.method, tc.path, w2.Body.String())
		}
	}
}

// TestCreateContractRequiresAdmin F4：创建期权合约是管理员操作，必须要求管理员角色。
func TestCreateContractRequiresAdmin(t *testing.T) {
	r, verifier, _ := newTestServer()
	body := `{"underlying":"BTC","strike":40000,"expiry":"2026-09-01T00:00:00Z","type":"call","style":"american","contract_size":1,"premium":1000}`
	adminTok := verifier.IssueRole(99, middleware.RoleAdmin, time.Hour)
	userTok := verifier.IssueRole(7, "user", time.Hour)

	// 非管理员：403。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/options/contracts", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+userTok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin create contract should be 403, got %d %s", w.Code, w.Body.String())
	}
	// 管理员：应成功创建（200）。
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/options/contracts", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+adminTok)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("admin create contract should succeed, got %d %s", w2.Code, w2.Body.String())
	}
}
