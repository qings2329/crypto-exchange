package user_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 本文件覆盖安全中心四组端点的契约：API Key / 登录历史 / 会话 / 防钓鱼码。

// doSec 发送带鉴权的请求并返回 recorder。
func doSec(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req, _ := http.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// unwrap 解包统一响应信封，返回 data 层。
func unwrap(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	data, _ := resp["data"].(map[string]interface{})
	return data
}

// TestApiKeyLifecycle 契约：创建（secret 仅一次）→ 列表 → 启停 → 删除。
func TestApiKeyLifecycle(t *testing.T) {
	r, _ := newTestHandler(t)
	tok := userAuth(1)

	// 1) label 缺失 → 400。
	w := doSec(t, r, http.MethodPost, "/api/v1/user/api-keys", tok, `{"permissions":["read"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing label should 400, got %d %s", w.Code, w.Body.String())
	}
	// 2) permissions 为空 → 400。
	w = doSec(t, r, http.MethodPost, "/api/v1/user/api-keys", tok, `{"label":"k1","permissions":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty permissions should 400, got %d %s", w.Code, w.Body.String())
	}
	// 3) 非法权限值 → 400。
	w = doSec(t, r, http.MethodPost, "/api/v1/user/api-keys", tok, `{"label":"k1","permissions":["root"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid permission should 400, got %d %s", w.Code, w.Body.String())
	}

	// 4) 正常创建：secret 仅本次返回，公钥形如 cxk_。
	body := `{"label":"交易机器人","permissions":["read","trade"],"ip_whitelist":["10.0.0.1"]}`
	w = doSec(t, r, http.MethodPost, "/api/v1/user/api-keys", tok, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	d := unwrap(t, w)
	ak := d["api_key"].(map[string]interface{})
	secret, _ := d["secret"].(string)
	if secret == "" || !strings.HasPrefix(ak["key"].(string), "cxk_") {
		t.Fatalf("expect secret + cxk_ key, got %s", w.Body.String())
	}
	if ak["status"] != "active" || ak["label"] != "交易机器人" {
		t.Fatalf("view fields wrong: %v", ak)
	}
	ips, _ := ak["ip_whitelist"].([]interface{})
	if len(ips) != 1 || ips[0] != "10.0.0.1" {
		t.Fatalf("ip_whitelist wrong: %v", ips)
	}
	keyID := strconv.FormatInt(int64(ak["id"].(float64)), 10)

	// 5) 列表：api_keys/total；不得再出现 secret 字段。
	w = doSec(t, r, http.MethodGet, "/api/v1/user/api-keys", tok, "")
	d = unwrap(t, w)
	arr := d["api_keys"].([]interface{})
	if d["total"].(float64) != 1 || len(arr) != 1 {
		t.Fatalf("list wrong: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatalf("secret must not leak in list: %s", w.Body.String())
	}

	// 6) 禁用 → ok；非法状态 → 400。
	w = doSec(t, r, http.MethodPut, "/api/v1/user/api-keys/"+keyID, tok, `{"status":"disabled"}`)
	if w.Code != http.StatusOK || unwrap(t, w)["ok"] != true {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	w = doSec(t, r, http.MethodPut, "/api/v1/user/api-keys/"+keyID, tok, `{"status":"hacked"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid status should 400, got %d", w.Code)
	}

	// 7) 删除 → ok；再删 → 404；他人密钥不可见。
	w = doSec(t, r, http.MethodDelete, "/api/v1/user/api-keys/"+keyID, tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	w = doSec(t, r, http.MethodDelete, "/api/v1/user/api-keys/"+keyID, tok, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("re-delete should 404, got %d", w.Code)
	}
	otherTok := userAuth(2)
	w = doSec(t, r, http.MethodDelete, "/api/v1/user/api-keys/"+keyID, otherTok, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-user delete should 404, got %d", w.Code)
	}
}

// TestLoginHistoryAndSessions 契约：登录成功记录历史+会话；失败也记历史；
// 会话 current 推导、不可注销当前会话、注销其余会话计数。
func TestLoginHistoryAndSessions(t *testing.T) {
	r, svc := newTestHandler(t)
	if _, err := svc.AdminCreate("sec@example.com", "password123"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	loginBody := `{"target":"sec@example.com","password":"password123"}`
	w := doSec(t, r, http.MethodPost, "/api/v1/user/login", "", loginBody)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	tok := "Bearer " + unwrap(t, w)["access_token"].(string)

	// 错误密码登录一次 → 失败历史。
	doSec(t, r, http.MethodPost, "/api/v1/user/login", "",
		`{"target":"sec@example.com","password":"wrong-pass"}`)

	// 登录历史：最新一条 success=false，且含成功记录；id 为字符串。
	w = doSec(t, r, http.MethodGet, "/api/v1/user/login-history", tok, "")
	d := unwrap(t, w)
	entries := d["entries"].([]interface{})
	if len(entries) != 2 {
		t.Fatalf("expect 2 login entries, got %s", w.Body.String())
	}
	first := entries[0].(map[string]interface{})
	if first["success"] != false {
		t.Fatalf("newest entry should be failure, got %v", first)
	}
	if _, ok := first["id"].(string); !ok {
		t.Fatalf("entry id must be string, got %T", first["id"])
	}

	// 会话列表：当前登录产生一条 current=true 的会话。
	w = doSec(t, r, http.MethodGet, "/api/v1/user/sessions", tok, "")
	d = unwrap(t, w)
	sessions := d["sessions"].([]interface{})
	if len(sessions) != 1 {
		t.Fatalf("expect 1 session, got %s", w.Body.String())
	}
	sess := sessions[0].(map[string]interface{})
	if sess["current"] != true {
		t.Fatalf("only session should be current: %v", sess)
	}
	curID := sess["id"].(string)

	// 注销当前会话 → 400。
	w = doSec(t, r, http.MethodDelete, "/api/v1/user/sessions/"+curID, tok, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("revoking current session should 400, got %d %s", w.Code, w.Body.String())
	}
	// 注销未知会话 → 404。
	w = doSec(t, r, http.MethodDelete, "/api/v1/user/sessions/nope", tok, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown session should 404, got %d", w.Code)
	}
	// 全部注销（保留当前）→ revoked=0。
	w = doSec(t, r, http.MethodDelete, "/api/v1/user/sessions", tok, "")
	if w.Code != http.StatusOK || unwrap(t, w)["revoked"].(float64) != 0 {
		t.Fatalf("revoke-all: %d %s", w.Code, w.Body.String())
	}
}

// TestAntiPhishingCode 契约：设置/读取/清除防钓鱼码，超长 400。
func TestAntiPhishingCode(t *testing.T) {
	r, _ := newTestHandler(t)
	tok := userAuth(1)

	// 未设置 → code="".
	w := doSec(t, r, http.MethodGet, "/api/v1/user/anti-phishing", tok, "")
	if c := unwrap(t, w)["code"].(string); c != "" {
		t.Fatalf("expect empty code, got %q", c)
	}

	// 设置 → message 提示已设置；读取回显。
	w = doSec(t, r, http.MethodPost, "/api/v1/user/anti-phishing", tok, `{"code":"CE-8888"}`)
	if w.Code != http.StatusOK || unwrap(t, w)["ok"] != true {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	w = doSec(t, r, http.MethodGet, "/api/v1/user/anti-phishing", tok, "")
	if c := unwrap(t, w)["code"].(string); c != "CE-8888" {
		t.Fatalf("expect CE-8888, got %q", c)
	}

	// 超长 → 400。
	long := strings.Repeat("x", 33)
	w = doSec(t, r, http.MethodPost, "/api/v1/user/anti-phishing", tok, `{"code":"`+long+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("too-long code should 400, got %d", w.Code)
	}

	// 清除（空串）→ code 回到空。
	w = doSec(t, r, http.MethodPost, "/api/v1/user/anti-phishing", tok, `{"code":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	w = doSec(t, r, http.MethodGet, "/api/v1/user/anti-phishing", tok, "")
	if c := unwrap(t, w)["code"].(string); c != "" {
		t.Fatalf("expect cleared, got %q", c)
	}
}
