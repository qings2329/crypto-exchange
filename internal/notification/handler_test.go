package notification

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

var testVerifier = middleware.NewTokenVerifier("test-secret")

func setupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthWithSkips(testVerifier, "/health"))
	svc := New(NewMemStore())
	h := NewHandler(svc)
	h.RegisterRoutes(r)
	return r
}

func authHeader(uid int64) string {
	return "Bearer " + testVerifier.Issue(uid, time.Hour)
}

func adminHeader() string {
	return "Bearer " + testVerifier.IssueAdmin(99, "admin", nil, time.Hour)
}

// respData 解包统一响应信封 {"code":0,"message":"ok","data":{...}}，返回 data 层。
func respData(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("no data envelope in resp %s", w.Body.String())
	}
	return data
}

func respCode(t *testing.T, w *httptest.ResponseRecorder) float64 {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	v, _ := resp["code"].(float64)
	return v
}

// --- 鉴权测试 ---

func TestHandlerListUnauthorized(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/list", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerCountUnauthorized(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/unread-count", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerReadUnauthorized(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{"id":1}`))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerReadAllUnauthorized(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/read-all", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerPublishUnauthorized(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(`{"title":"hi","body":"test"}`))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

// --- Health（免鉴权） ---

func TestHandlerHealth(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("expect status=ok, got %v", resp["status"])
	}
}

// --- Publish（发布通知） ---

func TestHandlerPublishHappy(t *testing.T) {
	r := setupRouter()
	body := `{"type":"system","title":"欢迎","body":"欢迎使用"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["title"] != "欢迎" {
		t.Fatalf("expect title=欢迎, got %v", data["title"])
	}
	if data["status"] != StatusUnread {
		t.Fatalf("expect status=unread, got %v", data["status"])
	}
}

func TestHandlerPublishMissingTitle(t *testing.T) {
	r := setupRouter()
	body := `{"type":"system","body":"no title"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerPublishInvalidBody(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(`{bad`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

// --- List（通知列表） ---

func TestHandlerListEmpty(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/list", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	items, _ := data["items"].([]interface{})
	if len(items) != 0 {
		t.Fatalf("expect 0 items, got %d", len(items))
	}
}

func TestHandlerListWithData(t *testing.T) {
	r := setupRouter()

	// 发布 2 条通知给 uid=1
	for _, title := range []string{"a", "b"} {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"` + title + `","body":"test"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("publish status=%d", w.Code)
		}
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/list", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	items, _ := data["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expect 2 items, got %d", len(items))
	}
	count, _ := data["count"].(float64)
	if int(count) != 2 {
		t.Fatalf("expect count=2, got %v", count)
	}
}

func TestHandlerListOnlyOtherUser(t *testing.T) {
	r := setupRouter()

	// 发布给 uid=2
	w := httptest.NewRecorder()
	body := `{"type":"system","title":"private","body":"for user 2"}`
	req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(2))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("publish status=%d", w.Code)
	}

	// uid=1 查看列表应为空
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/v1/notification/list", nil)
	req2.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expect 200, got %d", w2.Code)
	}
	data := respData(t, w2)
	items, _ := data["items"].([]interface{})
	if len(items) != 0 {
		t.Fatalf("expect 0 items for uid=1, got %d", len(items))
	}
}

func TestHandlerListUnreadFilter(t *testing.T) {
	r := setupRouter()

	// 发布 2 条通知
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"n","body":"b"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
	}

	// 标记其中一条已读
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{"id":1}`))
	req2.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("mark read status=%d", w2.Code)
	}

	// only_unread=true 应只剩 1 条
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("GET", "/api/v1/notification/list?only_unread=true", nil)
	req3.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w3, req3)
	if w3.Code != 200 {
		t.Fatalf("expect 200, got %d", w3.Code)
	}
	data := respData(t, w3)
	items, _ := data["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expect 1 unread item, got %d", len(items))
	}
}

func TestHandlerListLimit(t *testing.T) {
	r := setupRouter()

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"n","body":"b"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/list?limit=2", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	items, _ := data["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expect 2 items with limit=2, got %d", len(items))
	}
}

// --- UnreadCount ---

func TestHandlerUnreadCountZero(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/unread-count", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["unread"] != float64(0) {
		t.Fatalf("expect unread=0, got %v", data["unread"])
	}
}

func TestHandlerUnreadCountAfterPublish(t *testing.T) {
	r := setupRouter()

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"n","body":"b"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/unread-count", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	if data["unread"] != float64(3) {
		t.Fatalf("expect unread=3, got %v", data["unread"])
	}
}

func TestHandlerUnreadCountDecreasesAfterMarkRead(t *testing.T) {
	r := setupRouter()

	// 发布 2 条
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"n","body":"b"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
	}

	// 标记 id=1 已读
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{"id":1}`))
	req2.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w2, req2)

	// 未读应为 1
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("GET", "/api/v1/notification/unread-count", nil)
	req3.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w3, req3)
	data := respData(t, w3)
	if data["unread"] != float64(1) {
		t.Fatalf("expect unread=1, got %v", data["unread"])
	}
}

// --- MarkRead（标记已读） ---

func TestHandlerMarkReadHappy(t *testing.T) {
	r := setupRouter()

	// 先发布一条
	w := httptest.NewRecorder()
	body := `{"type":"system","title":"n","body":"b"}`
	req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)

	// 标记已读
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{"id":1}`))
	req2.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w2.Code, w2.Body.String())
	}
	data := respData(t, w2)
	if data["ok"] != true {
		t.Fatalf("expect ok=true, got %v", data["ok"])
	}
}

func TestHandlerMarkReadNotFound(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{"id":999}`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expect 404, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerMarkReadMissingID(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{}`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

func TestHandlerMarkReadInvalidJSON(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{bad`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

func TestHandlerMarkReadZeroID(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{"id":0}`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 for id=0, got %d", w.Code)
	}
}

// --- MarkAllRead（全部已读） ---

func TestHandlerMarkAllReadHappy(t *testing.T) {
	r := setupRouter()

	// 发布 3 条
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"n","body":"b"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
	}

	// 全部已读
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/notification/read-all", nil)
	req2.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w2.Code, w2.Body.String())
	}
	data := respData(t, w2)
	if data["ok"] != true {
		t.Fatalf("expect ok=true, got %v", data["ok"])
	}
	if data["marked"] != float64(3) {
		t.Fatalf("expect marked=3, got %v", data["marked"])
	}
}

func TestHandlerMarkAllReadZeroUnread(t *testing.T) {
	r := setupRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/notification/read-all", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	if data["marked"] != float64(0) {
		t.Fatalf("expect marked=0, got %v", data["marked"])
	}
}

func TestHandlerMarkAllReadThenUnreadZero(t *testing.T) {
	r := setupRouter()

	// 发布 2 条
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"n","body":"b"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
	}

	// 全部已读
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/notification/read-all", nil)
	req2.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w2, req2)

	// 未读数应为 0
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("GET", "/api/v1/notification/unread-count", nil)
	req3.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w3, req3)
	data := respData(t, w3)
	if data["unread"] != float64(0) {
		t.Fatalf("expect unread=0 after mark-all-read, got %v", data["unread"])
	}
}

// --- Admin ListAll ---

func TestHandlerAdminListAllHappy(t *testing.T) {
	r := setupRouter()

	// 为 uid=1 和 uid=2 各发布一条
	for _, uid := range []int64{1, 2} {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"n","body":"b"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(uid))
		r.ServeHTTP(w, req)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/admin/list", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	items, _ := data["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expect 2 items across users, got %d", len(items))
	}
}

func TestHandlerAdminListAllForbiddenForNonAdmin(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/admin/list", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403 for non-admin, got %d", w.Code)
	}
}

func TestHandlerAdminListAllUnauthorized(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/admin/list", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerAdminListAllLimit(t *testing.T) {
	r := setupRouter()

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		body := `{"type":"system","title":"n","body":"b"}`
		req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/notification/admin/list?limit=3", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	items, _ := data["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("expect 3 items with limit=3, got %d", len(items))
	}
}

// --- Cross-user isolation ---

func TestHandlerMarkReadOtherUserNotification(t *testing.T) {
	r := setupRouter()

	// uid=2 发布一条
	w := httptest.NewRecorder()
	body := `{"type":"system","title":"private","body":"for uid=2"}`
	req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(2))
	r.ServeHTTP(w, req)

	// uid=1 尝试标记 → 404（不属于他）
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/notification/read", strings.NewReader(`{"id":1}`))
	req2.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w2, req2)
	if w2.Code != 404 {
		t.Fatalf("expect 404 for cross-user mark read, got %d", w2.Code)
	}
}

// --- List 验证 user_id 字段 ---

func TestHandlerListContainsUserID(t *testing.T) {
	r := setupRouter()

	w := httptest.NewRecorder()
	body := `{"type":"system","title":"n","body":"b"}`
	req, _ := http.NewRequest("POST", "/api/v1/notification/publish", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(42))
	r.ServeHTTP(w, req)

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/v1/notification/list", nil)
	req2.Header.Set("Authorization", authHeader(42))
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expect 200, got %d", w2.Code)
	}
	data := respData(t, w2)
	uid := data["user_id"]
	if uid != float64(42) {
		t.Fatalf("expect user_id=42, got %v", uid)
	}
}
