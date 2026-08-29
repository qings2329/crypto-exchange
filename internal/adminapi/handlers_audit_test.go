package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAdminAuditLog(t *testing.T) {
	r, tok := newAdminServer(t)

	// 未携带 token -> 401
	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}

	// 携带 token 查询（初始应含 1 条本次登录产生的 login 审计）
	req = httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list audit-logs failed: %d %s", w.Code, w.Body.String())
	}
	var before struct {
		Data struct {
			Logs  []map[string]any `json:"logs"`
			Total int64            `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.Data.Total != 1 {
		t.Fatalf("expected 1 login audit entry initially, got %d", before.Data.Total)
	}
	if before.Data.Logs[0]["action"] != "login" {
		t.Fatalf("expected initial audit entry action 'login', got %v", before.Data.Logs[0]["action"])
	}

	// 执行一次变更操作（创建通知），应触发审计记录
	body, _ := json.Marshal(map[string]string{"title": "审计测试", "body": "x", "level": "info"})
	req = httptest.NewRequest(http.MethodPost, "/api/admin/notifications", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create notification failed: %d %s", w.Code, w.Body.String())
	}

	// 再次查询：应有 >=1 条，且最新一条为 create 通知
	req = httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs?limit=10", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list audit-logs failed: %d %s", w.Code, w.Body.String())
	}
	var after struct {
		Data struct {
			Logs  []gin.H `json:"logs"`
			Total int64   `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.Data.Total < 1 {
		t.Fatalf("expected >=1 audit entry after mutation, got %d", after.Data.Total)
	}
	latest := after.Data.Logs[0]
	if latest["action"] != "create" {
		t.Fatalf("expected latest audit action 'create', got %v", latest["action"])
	}
	if latest["path"] != "/api/admin/notifications" {
		t.Fatalf("expected latest audit path '/api/admin/notifications', got %v", latest["path"])
	}
	if int(latest["status"].(float64)) != http.StatusOK {
		t.Fatalf("expected latest audit status 200, got %v", latest["status"])
	}
}

// TestLoginAudit 验证登录事件（成功与失败）都会被写入审计日志。
// 登录路由位于 auditMiddleware 之外，须由 handleLogin 显式记录。
func TestLoginAudit(t *testing.T) {
	r, _ := newAdminServer(t) // 内部一次成功登录 -> 已产生 1 条 login 审计

	// 失败登录（错误密码）-> 应新增一条 login_failed
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password login should be 401, got %d", w.Code)
	}

	// 再次成功登录 -> 应再新增一条 login
	body2, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin123"})
	req = httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewReader(body2))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("good login should be 200, got %d", w.Code)
	}

	// 以返回的 token 查询审计日志，最新两条应分别为 login 与 login_failed
	var data struct {
		Data struct {
			Logs []map[string]any `json:"logs"`
		} `json:"data"`
	}
	var lg struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &lg); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs?limit=5", nil)
	req.Header.Set("Authorization", "Bearer "+lg.Data.Token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if err := json.Unmarshal(w2.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Data.Logs) < 2 {
		t.Fatalf("expected >=2 audit entries, got %d", len(data.Data.Logs))
	}
	if data.Data.Logs[0]["action"] != "login" {
		t.Fatalf("expected latest audit action 'login', got %v", data.Data.Logs[0]["action"])
	}
	if data.Data.Logs[1]["action"] != "login_failed" {
		t.Fatalf("expected second audit action 'login_failed', got %v", data.Data.Logs[1]["action"])
	}
	if data.Data.Logs[1]["status"].(float64) != http.StatusUnauthorized {
		t.Fatalf("expected login_failed audit status 401, got %v", data.Data.Logs[1]["status"])
	}
}
