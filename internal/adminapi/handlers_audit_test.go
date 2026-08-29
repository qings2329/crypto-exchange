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

// TestLoginAuditOnlySuccess 验证仅成功登录写入审计日志；失败登录不写入。
func TestLoginAuditOnlySuccess(t *testing.T) {
	r, loginTok := newAdminServer(t) // 内部一次成功登录 -> 已产生 1 条 login 审计
	tok := loginTok

	// 获取当前审计条数
	beforeCount := getAuditCount(t, r, tok)

	// 失败登录（错误密码）-> 不应新增审计条目
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password login should be 401, got %d", w.Code)
	}
	afterFailCount := getAuditCount(t, r, tok)
	if afterFailCount != beforeCount {
		t.Fatalf("failed login should not produce audit entry: before=%d after=%d", beforeCount, afterFailCount)
	}

	// 再次成功登录 -> 应新增一条 login 审计
	body2, _ := json.Marshal(map[string]string{"username": "admin", "password": "***REDACTED***"})
	req = httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewReader(body2))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("good login should be 200, got %d", w.Code)
	}
	afterSuccessCount := getAuditCount(t, r, tok)
	if afterSuccessCount != beforeCount+1 {
		t.Fatalf("successful login should add 1 audit entry: before=%d after=%d", beforeCount, afterSuccessCount)
	}
}

func getAuditCount(t *testing.T, r *gin.Engine, tok string) int {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var data struct {
		Data struct {
			Total int64 `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return int(data.Data.Total)
}
