package adminapi_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

// loginToken 用默认管理员凭据登录并返回 token（super_admin 拥有 apikey:* 权限）。
func loginToken(t *testing.T, r *gin.Engine) string {
	t.Helper()
	code, data := postJSON(t, r, "/api/admin/login", "", map[string]string{
		"username": "admin", "password": "admin123",
	})
	if code != http.StatusOK {
		t.Fatalf("login failed: %d", code)
	}
	m, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("login response unexpected: %#v", data)
	}
	tok, _ := m["token"].(string)
	if tok == "" {
		t.Fatal("empty token")
	}
	return tok
}

type apiKeyView struct {
	ID     int64  `json:"id"`
	UserID int64  `json:"user_id"`
	Label  string `json:"label"`
	Prefix string `json:"prefix"`
	Status string `json:"status"`
}

func TestAdminApiKeyCRUD(t *testing.T) {
	r, _ := newTestServer(t)
	tok := loginToken(t, r)

	// 1) 未带 token → 401
	code, _ := getJSON(t, r, "/api/admin/apikeys", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", code)
	}

	// 2) 创建 → 200，明文 key 一次性返回且 status=active
	code, data := postJSON(t, r, "/api/admin/apikeys", tok, map[string]interface{}{
		"user_id": 42, "label": "quant-bot", "permissions": []string{"read:market"},
	})
	if code != http.StatusOK {
		t.Fatalf("create api key: expected 200, got %d (data=%v)", code, data)
	}
	created, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected create response: %#v", data)
	}
	plain, _ := created["key"].(string)
	if plain == "" {
		t.Fatal("expected non-empty plaintext key")
	}
	rawView, _ := json.Marshal(created["api_key"])
	var view apiKeyView
	if err := json.Unmarshal(rawView, &view); err != nil {
		t.Fatalf("decode api_key view: %v", err)
	}
	if view.ID == 0 || view.Status != "active" || view.Prefix == "" {
		t.Fatalf("unexpected api_key view: %+v", view)
	}

	// 3) 列表 → 至少 1 条，total 一致
	code, data = getJSON(t, r, "/api/admin/apikeys", tok)
	if code != http.StatusOK {
		t.Fatalf("list api keys: expected 200, got %d", code)
	}
	list, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected list response: %#v", data)
	}
	items, _ := list["items"].([]interface{})
	total, _ := list["total"].(float64)
	if len(items) < 1 || int(total) != len(items) {
		t.Fatalf("expected list >=1 with matching total, got items=%d total=%v", len(items), total)
	}

	// 按 user_id 过滤
	code, data = getJSON(t, r, "/api/admin/apikeys?user_id=42", tok)
	if code != http.StatusOK {
		t.Fatalf("list filtered: expected 200, got %d", code)
	}
	list, _ = data.(map[string]interface{})
	items, _ = list["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 key for user 42, got %d", len(items))
	}

	// 4) 详情 → 200
	code, data = getJSON(t, r, "/api/admin/apikeys/"+strconv.FormatInt(view.ID, 10), tok)
	if code != http.StatusOK {
		t.Fatalf("get api key: expected 200, got %d", code)
	}

	// 5) 吊销 → 200
	code, _ = deleteJSON(t, r, "/api/admin/apikeys/"+strconv.FormatInt(view.ID, 10), tok)
	if code != http.StatusOK {
		t.Fatalf("revoke api key: expected 200, got %d", code)
	}

	// 6) 吊销后再查 → status=revoked；再吊销 → 409
	code, data = getJSON(t, r, "/api/admin/apikeys/"+strconv.FormatInt(view.ID, 10), tok)
	if code != http.StatusOK {
		t.Fatalf("get after revoke: expected 200, got %d", code)
	}
	raw2, _ := json.Marshal(data)
	var after apiKeyView
	_ = json.Unmarshal(raw2, &after)
	if after.Status != "revoked" {
		t.Fatalf("expected revoked status, got %q", after.Status)
	}
	code, _ = deleteJSON(t, r, "/api/admin/apikeys/"+strconv.FormatInt(view.ID, 10), tok)
	if code != http.StatusConflict {
		t.Fatalf("revoke twice: expected 409, got %d", code)
	}
}
