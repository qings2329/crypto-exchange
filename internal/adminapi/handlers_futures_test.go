package adminapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/gin-gonic/gin"
)

// newFuturesTestServer 启动记录请求并回显信封的假 futures 上游，把 admin 的 futures 基址指向它，
// 返回 *gin.Engine、super_admin token、以及假上游捕获。
func newFuturesTestServer(t *testing.T) (*gin.Engine, string, *riskCapture) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cap := &riskCapture{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r.Method, r.URL.Path, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gin.H{
			"code":    0,
			"message": "ok",
			"data": gin.H{
				"method": r.Method,
				"path":   r.URL.Path,
				"query":  r.URL.RawQuery,
			},
		})
	}))
	t.Cleanup(fake.Close)

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"futures": fake.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	_, data := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "***REDACTED***"})
	tok, _ := data.(map[string]interface{})["token"].(string)
	if tok == "" {
		t.Fatal("expected non-empty admin token")
	}
	return r, tok, cap
}

func TestFuturesManagementProxyForwards(t *testing.T) {
	r, tok, cap := newFuturesTestServer(t)

	checks := []struct {
		method, adminPath, upstreamPath, query string
	}{
		{http.MethodGet, "/api/admin/futures/positions", "/api/v1/futures/positions", ""},
		{http.MethodGet, "/api/admin/futures/positions?user_id=1", "/api/v1/futures/positions", "user_id=1"},
		{http.MethodGet, "/api/admin/futures/funding", "/api/v1/futures/funding", ""},
		{http.MethodGet, "/api/admin/futures/funding-history", "/api/v1/futures/funding-history", ""},
		{http.MethodPost, "/api/admin/futures/deposit", "/api/v1/futures/wallet/deposit", ""},
		{http.MethodPost, "/api/admin/futures/withdraw/chain", "/api/v1/futures/wallet/withdraw/chain", ""},
		{http.MethodPost, "/api/admin/futures/withdraw/emergency/freeze", "/api/v1/futures/wallet/withdraw/emergency/freeze", ""},
		{http.MethodPost, "/api/admin/futures/withdraw/emergency/resume", "/api/v1/futures/wallet/withdraw/emergency/resume", ""},
		{http.MethodPost, "/api/admin/futures/risk/enable", "/api/v1/futures/wallet/risk/enable", ""},
		{http.MethodPost, "/api/admin/futures/baddebt/socialize/propose", "/api/v1/futures/wallet/baddebt/socialize/propose", ""},
		{http.MethodPost, "/api/admin/futures/baddebt/socialize/approve", "/api/v1/futures/wallet/baddebt/socialize/approve", ""},
	}
	for _, c := range checks {
		var code int
		var _ interface{}
		switch c.method {
		case http.MethodGet:
			code, _ = getJSON(t, r, c.adminPath, tok)
		case http.MethodPost:
			code, _ = postJSON(t, r, c.adminPath, tok, map[string]string{"dummy": "1"})
		}
		if code != http.StatusOK {
			t.Fatalf("%s %s: expected 200, got %d", c.method, c.adminPath, code)
		}
		m, p, q := cap.last()
		if m != c.method || p != c.upstreamPath {
			t.Fatalf("%s %s forwarded wrong: method=%s path=%s (want %s %s)", c.method, c.adminPath, m, p, c.method, c.upstreamPath)
		}
		if c.query != "" && q != c.query {
			t.Fatalf("%s %s query mismatch: got %q want %q", c.method, c.adminPath, q, c.query)
		}
	}
}

// TestFuturesManagementRBAC 验证：仅 futures:view 的 token 可读不可写（管理操作应 403）。
func TestFuturesManagementRBAC(t *testing.T) {
	r, _, _ := newFuturesTestServer(t)

	v := middleware.NewTokenVerifier("test-secret")
	viewOnly := v.IssueAdmin(3, "admin", []string{adminapi.PermFuturesView}, time.Hour)

	// 读：应通过。
	code, _ := getJSON(t, r, "/api/admin/futures/positions", viewOnly)
	if code != http.StatusOK {
		t.Fatalf("futures:view should allow GET /futures/positions, got %d", code)
	}

	// 写（代客直提）：应被 RequirePerm(futures:manage) 拒绝（403）。
	code, _ = postJSON(t, r, "/api/admin/futures/withdraw/chain", viewOnly, map[string]string{"user_id": "1"})
	if code != http.StatusForbidden {
		t.Fatalf("futures:view without futures:manage should be 403 on POST /futures/withdraw/chain, got %d", code)
	}

	// 写（应急冻结）：应 403。
	code, _ = postJSON(t, r, "/api/admin/futures/withdraw/emergency/freeze", viewOnly, nil)
	if code != http.StatusForbidden {
		t.Fatalf("futures:view without futures:manage should be 403 on POST /futures/withdraw/emergency/freeze, got %d", code)
	}
}
