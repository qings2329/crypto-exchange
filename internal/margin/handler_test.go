package margin

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

// newTestServer 起一个真实 gin 引擎并注册 margin 路由，返回引擎与签发 token 的 verifier。
func newTestServer() (*gin.Engine, *middleware.TokenVerifier, *Service) {
	gin.SetMode(gin.TestMode)
	store := NewMemStore()
	l := ledger.New()
	for _, uid := range []int64{1, 2} {
		_ = l.Deposit(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed")
	}
	cfg := Config{MaxLeverage: 5, HourlyRate: 0.0001, MaintenanceRatio: 1.05, CollateralAsset: "USDT", AccrueInterval: time.Second}
	svc := NewService(store, l, cfg, zap.NewNop(), nil)
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	svc.RegisterRoutes(r, verifier)
	return r, verifier, svc
}

// TestLiquidateRequiresAdmin F4：强制清算端点必须要求管理员角色，否则任意登录用户可强平他人账户。
func TestLiquidateRequiresAdmin(t *testing.T) {
	r, verifier, _ := newTestServer()
	adminTok := verifier.IssueRole(99, middleware.RoleAdmin, time.Hour)
	userTok := verifier.IssueRole(7, "user", time.Hour)

	body := `{"user_id":1,"asset":"BTC"}`
	// 非管理员：必须 403。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/margin/liquidate", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+userTok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin liquidate should be 403, got %d %s", w.Code, w.Body.String())
	}
	// 管理员：通过护栏（到达 handler，业务结果取决于账户是否存在，不得为 403）。
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/margin/liquidate", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+adminTok)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code == http.StatusForbidden {
		t.Fatalf("admin liquidate should pass guard, got 403 %s", w2.Body.String())
	}
}

// TestUserEndpointsUseTokenIdentity F4：用户态端点以 token 身份为准，忽略请求体中的 user_id（防 IDOR）。
func TestUserEndpointsUseTokenIdentity(t *testing.T) {
	r, verifier, svc := newTestServer()
	// 以 user=1 的 token 发 borrow，请求体故意写 user_id=2（越权尝试）。
	user1Tok := verifier.IssueRole(1, "user", time.Hour)
	body := `{"user_id":2,"asset":"BTC","amount":1.0,"leverage":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/margin/borrow", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+user1Tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("borrow should succeed, got %d %s", w.Code, w.Body.String())
	}
	// 账户应建在 token 主体 user=1 名下，而非请求体里的 user_id=2。
	if list, _ := svc.Accounts(1); len(list) != 1 {
		t.Fatalf("expected account under token uid=1, got %d", len(list))
	}
	if list, _ := svc.Accounts(2); len(list) != 0 {
		t.Fatalf("must NOT create account under body user_id=2, got %d", len(list))
	}
}
