package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

// newAdminServer 起一个内存态（无 MySQL）的管理后端，登录后返回 router 与 admin token。
func newAdminServer(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "admin123"
	cfg.Admin.TokenTTLSec = 3600
	// MySQL.DSN 留空 -> 公告存储回退内存。

	r := gin.New()
	NewServer(cfg).RegisterRoutes(r)

	b, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin123"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin login failed: %d %s", w.Code, w.Body.String())
	}
	var login struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	return r, login.Data.Token
}

type annEnv struct {
	Data struct {
		Announcements []struct {
			ID     int64  `json:"id"`
			Level  string `json:"level"`
			Title  string `json:"title"`
			Active bool   `json:"active"`
		} `json:"announcements"`
	} `json:"data"`
}

func TestAdminAnnouncementCRUD(t *testing.T) {
	r, tok := newAdminServer(t)

	// 未携带 token -> 401
	req := httptest.NewRequest(http.MethodGet, "/api/admin/announcements", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}

	// 创建公告
	body, _ := json.Marshal(map[string]interface{}{
		"level":   "warning",
		"title":   "系统维护通知",
		"content": "今晚 22:00 起维护 1 小时",
		"active":  true,
	})
	req = httptest.NewRequest(http.MethodPost, "/api/admin/announcements", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create announcement failed: %d %s", w.Code, w.Body.String())
	}
	// adminCreate 直接返回公告对象（response.JSON(c, a)），故 data 为单个公告而非 announcements 数组。
	var created struct {
		Data struct {
			ID     int64  `json:"id"`
			Level  string `json:"level"`
			Title  string `json:"title"`
			Active bool   `json:"active"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.ID == 0 {
		t.Fatalf("create response missing announcement")
	}
	id := created.Data.ID

	// 列表应含刚创建的公告
	req = httptest.NewRequest(http.MethodGet, "/api/admin/announcements", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list announcements failed: %d", w.Code)
	}
	var list annEnv
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data.Announcements) == 0 {
		t.Fatalf("expected at least 1 announcement")
	}

	// 更新公告
	upd, _ := json.Marshal(map[string]interface{}{
		"title":  "系统维护通知（时间调整）",
		"active": false,
	})
	req = httptest.NewRequest(http.MethodPut, "/api/admin/announcements/"+strconv.FormatInt(id, 10), bytes.NewReader(upd))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update announcement failed: %d %s", w.Code, w.Body.String())
	}

	// 删除公告
	req = httptest.NewRequest(http.MethodDelete, "/api/admin/announcements/"+strconv.FormatInt(id, 10), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete announcement failed: %d %s", w.Code, w.Body.String())
	}

	// 删除后列表应为空
	req = httptest.NewRequest(http.MethodGet, "/api/admin/announcements", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list announcements failed: %d", w.Code)
	}
	var after annEnv
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Data.Announcements) != 0 {
		t.Fatalf("expected 0 announcements after delete, got %d", len(after.Data.Announcements))
	}
}
