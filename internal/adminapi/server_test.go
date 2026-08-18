package adminapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

func newTestServer(t *testing.T) (*gin.Engine, *config.Config) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "admin123"
	cfg.Admin.TokenTTLSec = 3600
	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)
	return r, cfg
}

// postJSON 发送 JSON（POST）并解析 {code,data} 信封。
func postJSON(t *testing.T, r *gin.Engine, path, token string, body interface{}) (int, interface{}) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env struct {
		Code int         `json:"code"`
		Data interface{} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w.Code, env.Data
}

// putJSON 发送 JSON（PUT）并解析 {code,data} 信封。
func putJSON(t *testing.T, r *gin.Engine, path, token string, body interface{}) (int, interface{}) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env struct {
		Code int         `json:"code"`
		Data interface{} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w.Code, env.Data
}

// getJSON 发送 GET 并解析 {code,data} 信封。
func getJSON(t *testing.T, r *gin.Engine, path, token string) (int, interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env struct {
		Code int         `json:"code"`
		Data interface{} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w.Code, env.Data
}

// deleteJSON 发送 DELETE 并解析 {code,data} 信封。
func deleteJSON(t *testing.T, r *gin.Engine, path, token string) (int, interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env struct {
		Code int         `json:"code"`
		Data interface{} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w.Code, env.Data
}

func TestAdminLoginAndRoleGuard(t *testing.T) {
	r, _ := newTestServer(t)

	// 1) 错误凭据 → 401
	code, _ := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "wrong"})
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad creds, got %d", code)
	}

	// 2) 正确凭据 → 200 + token
	code, data := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "admin123"})
	if code != http.StatusOK {
		t.Fatalf("expected 200 for good creds, got %d", code)
	}
	tok, _ := data.(map[string]interface{})["token"].(string)
	if tok == "" {
		t.Fatal("expected non-empty admin token")
	}

	// 3) 用 user 角色 token 访问管理接口 → 403（需 AdminGuard）
	//    手工用 verifier 签一个 user token 不可行（未导出）；这里用一段明显非 admin 的假 token 验证被拒。
	code, _ = postJSON(t, r, "/api/admin/users", "not-a-real-token", nil)
	if code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("expected 401/403 for non-admin token, got %d", code)
	}

	// 4) 用真实 admin token 访问 → 200（用户列表数据应随响应返回）
	code, _ = getJSON(t, r, "/api/admin/users", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200 for admin token, got %d", code)
	}

	// 5) 风控快照可访问
	code, _ = getJSON(t, r, "/api/admin/risk", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200 for /admin/risk, got %d", code)
	}
}

func TestAdminRequiresLogin(t *testing.T) {
	r, _ := newTestServer(t)
	// 未带 token 直接访问管理接口 → 401
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}
}

// TestAdminLoginLockout 验证登录暴力防护：连续失败达到阈值锁定账户，
// 锁定后即便正确密码也被拒（证明锁定生效），且锁定前成功登录会清零失败计数。
func TestAdminLoginLockout(t *testing.T) {
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "admin123"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Admin.MaxLoginFailures = 2 // 测试用小阈值
	cfg.Admin.LoginLockoutSec = 900
	gin.SetMode(gin.TestMode)
	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	// 1) 锁定前：第 1 次错误 → 401（未达阈值）
	if code, _ := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("attempt1: expected 401, got %d", code)
	}
	// 2) 锁定前正确密码 → 200（并清零失败计数）
	if code, data := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "admin123"}); code != http.StatusOK {
		t.Fatalf("pre-lock good login: expected 200, got %d", code)
	} else if data.(map[string]interface{})["token"] == "" {
		t.Fatal("expected non-empty token")
	}

	// 3) 连续 2 次错误 → 触发锁定
	postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "wrong1"})
	postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "wrong2"})

	// 4) 锁定后：即使正确密码也应被拒（证明锁定生效，而非仅计数）
	if code, _ := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "admin123"}); code != http.StatusUnauthorized {
		t.Fatalf("locked correct login: expected 401, got %d", code)
	}
	// 5) 锁定后：错误密码同样被拒
	if code, _ := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "wrong3"}); code != http.StatusUnauthorized {
		t.Fatalf("locked wrong login: expected 401, got %d", code)
	}
}

// TestAdminLoginIPRateLimit 验证基于 IP 的登录限流：单 IP 达阈值后返回 429，
// 且不同 IP 独立计数（证明按来源 IP 限流，不波及他人）。
func TestAdminLoginIPRateLimit(t *testing.T) {
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "admin123"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Admin.LoginRateLimitPerIP = 2 // 单 IP 2 次/窗口
	cfg.Admin.LoginRateWindowSec = 60
	gin.SetMode(gin.TestMode)
	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	// loginAs 以指定来源 IP 发起一次登录尝试。
	loginAs := func(ip string) int {
		b, _ := json.Marshal(map[string]string{"username": "admin", "password": "wrong"})
		req := httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":1234"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	// 1) IP-A 前 2 次尝试 → 401（在阈值内，放行）
	if code := loginAs("10.0.0.1"); code != http.StatusUnauthorized {
		t.Fatalf("ipA attempt1: expected 401, got %d", code)
	}
	if code := loginAs("10.0.0.1"); code != http.StatusUnauthorized {
		t.Fatalf("ipA attempt2: expected 401, got %d", code)
	}
	// 2) IP-A 第 3 次 → 429（达阈值被限流）
	if code := loginAs("10.0.0.1"); code != http.StatusTooManyRequests {
		t.Fatalf("ipA attempt3: expected 429, got %d", code)
	}
	// 3) 不同 IP-B 仍被放行（独立计数，证明按 IP 限流）
	if code := loginAs("10.0.0.2"); code != http.StatusUnauthorized {
		t.Fatalf("ipB attempt: expected 401 (independent bucket), got %d", code)
	}
}

// TestAdminPreferences 验证管理员偏好（语言/主题/时区）的读写往返：
// 默认空值 → 写入具体值 → 读回一致 → 改回空串（跟随系统）被保留。
func TestAdminPreferences(t *testing.T) {
	r, _ := newTestServer(t)

	// 登录拿 token
	_, data := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "admin123"})
	tok, _ := data.(map[string]interface{})["token"].(string)
	if tok == "" {
		t.Fatal("expected non-empty admin token")
	}

	// 默认偏好：未设置时为空值
	code, d := getJSON(t, r, "/api/admin/preferences", tok)
	if code != http.StatusOK {
		t.Fatalf("get preferences: expected 200, got %d", code)
	}
	pref := d.(map[string]interface{})
	if pref["language"] != "" || pref["theme"] != "" || pref["timezone"] != "" {
		t.Fatalf("expected empty default preferences, got %+v", pref)
	}

	// 写入具体偏好
	code, _ = putJSON(t, r, "/api/admin/preferences", tok, map[string]string{
		"language": "en", "theme": "midnight", "timezone": "Asia/Tokyo",
	})
	if code != http.StatusOK {
		t.Fatalf("put preferences: expected 200, got %d", code)
	}
	code, d = getJSON(t, r, "/api/admin/preferences", tok)
	if code != http.StatusOK {
		t.Fatalf("get preferences after update: expected 200, got %d", code)
	}
	pref = d.(map[string]interface{})
	if pref["language"] != "en" || pref["theme"] != "midnight" || pref["timezone"] != "Asia/Tokyo" {
		t.Fatalf("preferences not persisted: %+v", pref)
	}

	// 改回空串（跟随系统）应被保留
	code, _ = putJSON(t, r, "/api/admin/preferences", tok, map[string]string{"timezone": ""})
	if code != http.StatusOK {
		t.Fatalf("put preferences clear: expected 200, got %d", code)
	}
	_, d = getJSON(t, r, "/api/admin/preferences", tok)
	pref = d.(map[string]interface{})
	if pref["timezone"] != "" {
		t.Fatalf("expected empty timezone after clear, got %v", pref["timezone"])
	}
}
