package announcement

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// 响应信封（与 internal/pkg/response 约定一致）。
type env struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// setupRouter 装配公告路由（使用内存存储，无需数据库），并返回 admin/user token 便于鉴权测试。
func setupRouter() (*gin.Engine, *middleware.TokenVerifier, string, string) {
	gin.SetMode(gin.TestMode)
	verifier := middleware.NewTokenVerifier("test-secret-ann")
	svc := NewService(NewMemStore())
	h := NewHandler(svc)
	r := gin.New()
	h.Register(r, verifier)
	adminToken := verifier.IssueAdmin(1, "admin", nil, time.Hour)
	userToken := verifier.Issue(2, time.Hour) // 普通用户（非 admin）
	return r, verifier, adminToken, userToken
}

func doReq(r http.Handler, method, path, token string, body interface{}) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeEnv(t *testing.T, w *httptest.ResponseRecorder) env {
	t.Helper()
	var e env
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, w.Body.String())
	}
	return e
}

// 公开列表免鉴权，且返回 announcements 数组。
func TestHandlerPublicListNoAuth(t *testing.T) {
	r, _, _, _ := setupRouter()
	w := doReq(r, http.MethodGet, "/api/v1/announcement/list", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	e := decodeEnv(t, w)
	if e.Code != 0 {
		t.Fatalf("expected code 0, got %d (%s)", e.Code, e.Message)
	}
	if !strings.Contains(w.Body.String(), "announcements") {
		t.Fatalf("expected announcements field, body=%s", w.Body.String())
	}
}

// 管理接口缺 Token → 401。
func TestHandlerAdminRequiresAuth(t *testing.T) {
	r, _, _, _ := setupRouter()
	w := doReq(r, http.MethodPost, "/api/v1/announcement/admin", "", map[string]string{"title": "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// 管理接口普通用户 Token（非 admin）→ 403。
func TestHandlerAdminRequiresRole(t *testing.T) {
	r, _, _, userToken := setupRouter()
	w := doReq(r, http.MethodPost, "/api/v1/announcement/admin", userToken, map[string]string{"title": "x"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// admin Token 可创建公告，返回 200 且含 id。
func TestHandlerAdminCreate(t *testing.T) {
	r, _, adminToken, _ := setupRouter()
	w := doReq(r, http.MethodPost, "/api/v1/announcement/admin", adminToken, map[string]interface{}{
		"level":   LevelMaintenance,
		"title":   "维护通知",
		"content": "今晚维护",
		"active":  true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	e := decodeEnv(t, w)
	if e.Code != 0 {
		t.Fatalf("expected code 0, got %d (%s)", e.Code, e.Message)
	}
	var created Announcement
	if err := json.Unmarshal(e.Data, &created); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if created.ID == 0 || created.Level != LevelMaintenance || !created.Active {
		t.Fatalf("unexpected created: %+v", created)
	}
}

// admin Token 创建非法 level → 400。
func TestHandlerAdminCreateInvalidLevel(t *testing.T) {
	r, _, adminToken, _ := setupRouter()
	w := doReq(r, http.MethodPost, "/api/v1/announcement/admin", adminToken, map[string]interface{}{
		"level": "critical", "title": "x",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// 更新/删除不存在的 id → 404。
func TestHandlerAdminUpdateNotFound(t *testing.T) {
	r, _, adminToken, _ := setupRouter()
	w := doReq(r, http.MethodPut, "/api/v1/announcement/admin/999", adminToken, map[string]string{"title": "x"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandlerAdminDeleteNotFound(t *testing.T) {
	r, _, adminToken, _ := setupRouter()
	w := doReq(r, http.MethodDelete, "/api/v1/announcement/admin/999", adminToken, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// 完整管理流：创建 → 列表含之 → 更新 → 删除后不在列表。
func TestHandlerAdminLifecycle(t *testing.T) {
	r, _, adminToken, _ := setupRouter()

	// 创建
	cw := doReq(r, http.MethodPost, "/api/v1/announcement/admin", adminToken, map[string]interface{}{
		"title": "生命周期", "active": true,
	})
	if cw.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d", cw.Code)
	}
	var created Announcement
	_ = json.Unmarshal(decodeEnv(t, cw).Data, &created)
	if created.ID == 0 {
		t.Fatal("expected non-zero id")
	}

	// 管理全量列表含之
	lw := doReq(r, http.MethodGet, "/api/v1/announcement/admin", adminToken, nil)
	var listEnv struct {
		Announcements []Announcement `json:"announcements"`
	}
	_ = json.Unmarshal(decodeEnv(t, lw).Data, &listEnv)
	found := false
	for _, a := range listEnv.Announcements {
		if a.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("created announcement missing from admin list")
	}

	// 更新标题
	uw := doReq(r, http.MethodPut, "/api/v1/announcement/admin/"+itoa(created.ID), adminToken, map[string]interface{}{"title": "改后"})
	if uw.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d", uw.Code)
	}
	var updated Announcement
	_ = json.Unmarshal(decodeEnv(t, uw).Data, &updated)
	if updated.Title != "改后" {
		t.Fatalf("expected title updated, got %q", updated.Title)
	}

	// 删除
	dw := doReq(r, http.MethodDelete, "/api/v1/announcement/admin/"+itoa(created.ID), adminToken, nil)
	if dw.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", dw.Code)
	}
	// 删除后再查应 404
	gw := doReq(r, http.MethodPut, "/api/v1/announcement/admin/"+itoa(created.ID), adminToken, map[string]interface{}{"title": "x"})
	if gw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", gw.Code)
	}
}

func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
