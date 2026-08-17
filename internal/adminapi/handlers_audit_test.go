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

	// 携带 token 查询（初始应为空）
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
	if before.Data.Total != 0 {
		t.Fatalf("expected empty audit log initially, got %d", before.Data.Total)
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
