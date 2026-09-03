package adminapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// newRiskTestServer 启动一个记录请求方法/路径并回显信封的假 risk 上游，把 admin 管理后台的
// risk 服务基址指向它，返回 *gin.Engine、登录拿到的 super_admin token、以及假上游捕获的请求。
func newRiskTestServer(t *testing.T) (*gin.Engine, string, *riskCapture) {
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
	cfg.Services = map[string]string{"risk": fake.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	// 登录拿 super_admin token。
	_, data := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "***REDACTED***"})
	tok, _ := data.(map[string]interface{})["token"].(string)
	if tok == "" {
		t.Fatal("expected non-empty admin token")
	}
	return r, tok, cap
}

// riskCapture 记录假 risk 上游收到的请求，便于断言代理转发正确。
type riskCapture struct {
	methods []string
	paths   []string
	queries []string
}

func (c *riskCapture) record(method, path, query string) {
	c.methods = append(c.methods, method)
	c.paths = append(c.paths, path)
	c.queries = append(c.queries, query)
}

// last 返回最近一次捕获的请求三元组。
func (c *riskCapture) last() (string, string, string) {
	i := len(c.methods) - 1
	return c.methods[i], c.paths[i], c.queries[i]
}

func TestRiskManagementProxyForwards(t *testing.T) {
	r, tok, cap := newRiskTestServer(t)

	// 规则列表 GET
	code, _ := getJSON(t, r, "/api/admin/risk/rules", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /risk/rules: expected 200, got %d", code)
	}
	if m, p, _ := cap.last(); m != http.MethodGet || p != "/api/v1/risk/rules" {
		t.Fatalf("rules list forwarded wrong: method=%s path=%s", m, p)
	}

	// 规则创建 POST
	code, _ = postJSON(t, r, "/api/admin/risk/rules", tok, map[string]string{"kind": "withdraw_limit", "asset": "BTC"})
	if code != http.StatusOK {
		t.Fatalf("POST /risk/rules: expected 200, got %d", code)
	}
	if m, p, _ := cap.last(); m != http.MethodPost || p != "/api/v1/risk/rules" {
		t.Fatalf("rule create forwarded wrong: method=%s path=%s", m, p)
	}

	// 黑名单列表 GET（带 kind query）
	code, _ = getJSON(t, r, "/api/admin/risk/blacklist?kind=address", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /risk/blacklist: expected 200, got %d", code)
	}
	if m, p, q := cap.last(); m != http.MethodGet || p != "/api/v1/risk/blacklist" || !strings.Contains(q, "kind=address") {
		t.Fatalf("blacklist list forwarded wrong: method=%s path=%s query=%s", m, p, q)
	}

	// 黑名单创建 POST
	code, _ = postJSON(t, r, "/api/admin/risk/blacklist", tok, map[string]string{"target": "0xbad", "kind": "address", "reason": "sanctioned"})
	if code != http.StatusOK {
		t.Fatalf("POST /risk/blacklist: expected 200, got %d", code)
	}

	// 黑名单移除 DELETE（带 target query）
	code, _ = deleteJSON(t, r, "/api/admin/risk/blacklist?target=0xbad", tok)
	if code != http.StatusOK {
		t.Fatalf("DELETE /risk/blacklist: expected 200, got %d", code)
	}
	if m, p, q := cap.last(); m != http.MethodDelete || p != "/api/v1/risk/blacklist" || !strings.Contains(q, "target=0xbad") {
		t.Fatalf("blacklist delete forwarded wrong: method=%s path=%s query=%s", m, p, q)
	}

	// 黑名单查询 GET
	code, _ = getJSON(t, r, "/api/admin/risk/blacklist/check?target=0xbad", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /risk/blacklist/check: expected 200, got %d", code)
	}
	if m, p, q := cap.last(); m != http.MethodGet || p != "/api/v1/risk/blacklist/check" || !strings.Contains(q, "target=0xbad") {
		t.Fatalf("blacklist check forwarded wrong: method=%s path=%s query=%s", m, p, q)
	}

	// 提现风控预检 POST
	code, _ = postJSON(t, r, "/api/admin/risk/check/withdraw", tok, map[string]interface{}{"user_id": 1, "asset": "BTC", "amount": 10})
	if code != http.StatusOK {
		t.Fatalf("POST /risk/check/withdraw: expected 200, got %d", code)
	}
	if m, p, _ := cap.last(); m != http.MethodPost || p != "/api/v1/risk/check/withdraw" {
		t.Fatalf("withdraw check forwarded wrong: method=%s path=%s", m, p)
	}

	// 风控事件列表 GET（真实告警源，带 limit/user_id query）
	code, _ = getJSON(t, r, "/api/admin/risk/events?limit=20&user_id=7", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /risk/events: expected 200, got %d", code)
	}
	if m, p, q := cap.last(); m != http.MethodGet || p != "/api/v1/risk/events" || !strings.Contains(q, "limit=20") || !strings.Contains(q, "user_id=7") {
		t.Fatalf("risk events forwarded wrong: method=%s path=%s query=%s", m, p, q)
	}
}

// TestRiskManagementRBAC 验证：仅 risk:view 的 token 可读但不可写（增删规则/黑名单应 403）。
func TestRiskManagementRBAC(t *testing.T) {
	r, _, _ := newRiskTestServer(t)

	// 仅含 risk:view 的受限 token（无 risk:manage）。
	v := middleware.NewTokenVerifier("test-secret")
	viewOnly := v.IssueAdmin(2, "admin", []string{adminapi.PermRiskView}, time.Hour)

	// 读：应通过。
	code, _ := getJSON(t, r, "/api/admin/risk/rules", viewOnly)
	if code != http.StatusOK {
		t.Fatalf("risk:view should allow GET /risk/rules, got %d", code)
	}

	// 读风控事件（真实告警源）：也应通过（仅需 risk:view）。
	code, _ = getJSON(t, r, "/api/admin/risk/events?limit=10", viewOnly)
	if code != http.StatusOK {
		t.Fatalf("risk:view should allow GET /risk/events, got %d", code)
	}

	// 写（创建规则）：应被 RequirePerm(risk:manage) 拒绝（403）。
	code, _ = postJSON(t, r, "/api/admin/risk/rules", viewOnly, map[string]string{"kind": "withdraw_limit"})
	if code != http.StatusForbidden {
		t.Fatalf("risk:view without risk:manage should be 403 on POST /risk/rules, got %d", code)
	}

	// 写（删除黑名单）：应 403。
	code, _ = deleteJSON(t, r, "/api/admin/risk/blacklist?target=0xbad", viewOnly)
	if code != http.StatusForbidden {
		t.Fatalf("risk:view without risk:manage should be 403 on DELETE /risk/blacklist, got %d", code)
	}
}
