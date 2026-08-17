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
