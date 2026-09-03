package c2c

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

// newUserServer 注册用户侧 C2C 路由（仅需登录）。
func newUserServer() (*gin.Engine, *middleware.TokenVerifier, *Service) {
	gin.SetMode(gin.TestMode)
	verifier := middleware.NewTokenVerifier("test-secret")
	svc := NewService(NewMemStore())
	h := NewHandler(svc, verifier)
	r := gin.New()
	h.Register(r)
	return r, verifier, svc
}

func doReq(t *testing.T, r http.Handler, method, path, body, token string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]interface{}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "json") {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

func TestCreateRequiresAuth(t *testing.T) {
	r, _, _ := newUserServer()
	code, _ := doReq(t, r, http.MethodPost, "/api/v1/c2c/orders", `{"side":"buy","coin":"USDT","amount":100,"price":7.2}`, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create: got %d, want 401", code)
	}
}

func TestCreateAndMyOrders(t *testing.T) {
	r, verifier, _ := newUserServer()
	tokA := verifier.IssueRole(10, "user", time.Hour)
	tokB := verifier.IssueRole(20, "user", time.Hour)

	code, body := doReq(t, r, http.MethodPost, "/api/v1/c2c/orders", `{"side":"buy","coin":"USDT","amount":100,"price":7.2}`, tokA)
	if code != http.StatusOK {
		t.Fatalf("create: got %d, want 200 (body=%v)", code, body)
	}
	data, _ := body["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("create: missing data envelope (body=%v)", body)
	}
	o, _ := data["order"].(map[string]interface{})
	if o["user_id"].(float64) != 10 {
		t.Fatalf("created order user_id = %v, want 10", o["user_id"])
	}
	if o["status"].(string) != string(StatusOpen) {
		t.Fatalf("created order status = %v, want open", o["status"])
	}

	// 另一个用户下单
	if code, _ := doReq(t, r, http.MethodPost, "/api/v1/c2c/orders", `{"side":"sell","coin":"BTC","amount":1,"price":390000}`, tokB); code != http.StatusOK {
		t.Fatalf("user B create: got %d", code)
	}

	// 用户 A 只能看到自己的订单（数据隔离）
	code, body = doReq(t, r, http.MethodGet, "/api/v1/c2c/orders", "", tokA)
	if code != http.StatusOK {
		t.Fatalf("list: got %d, want 200", code)
	}
	itemsData, _ := body["data"].(map[string]interface{})
	items, _ := itemsData["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("user A expected 1 order, got %d", len(items))
	}
	for _, it := range items {
		m := it.(map[string]interface{})
		if m["user_id"].(float64) != 10 {
			t.Fatalf("leak: order user_id = %v, want own only", m["user_id"])
		}
	}
}

func TestCreateValidationViaHTTP(t *testing.T) {
	r, verifier, _ := newUserServer()
	tok := verifier.IssueRole(1, "user", time.Hour)

	code, _ := doReq(t, r, http.MethodPost, "/api/v1/c2c/orders", `{"side":"hold","coin":"USDT","amount":100,"price":7.2}`, tok)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid side: got %d, want 400", code)
	}
	code, _ = doReq(t, r, http.MethodPost, "/api/v1/c2c/orders", `{"side":"buy","coin":"USDT","amount":0,"price":7.2}`, tok)
	if code != http.StatusBadRequest {
		t.Fatalf("zero amount: got %d, want 400", code)
	}
}

func TestAdminActionRequiresAdmin(t *testing.T) {
	verifier := middleware.NewTokenVerifier("test-secret")
	userTok := verifier.IssueRole(5, "user", time.Hour)
	adminTok := verifier.IssueRole(99, middleware.RoleAdmin, time.Hour)

	// 预置一笔 open 订单供管理动作测试。
	sm := NewMemStore()
	preseed := &Order{Side: SideSell, Coin: "BTC", Amount: 0.5, Price: 398000, UserID: 5, Status: StatusOpen}
	if err := sm.Create(preseed); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(NewService(sm), verifier)
	r := gin.New()
	h.RegisterAdmin(r)
	id := preseed.ID

	// 非管理员执行冻结应 403
	code, _ := doReq(t, r, http.MethodPost, "/api/v1/c2c/orders/"+strconv.FormatInt(id, 10)+"/freeze", "", userTok)
	if code != http.StatusForbidden {
		t.Fatalf("non-admin freeze: got %d, want 403", code)
	}

	// 管理员可冻结
	code, body := doReq(t, r, http.MethodPost, "/api/v1/c2c/orders/"+strconv.FormatInt(id, 10)+"/freeze", "", adminTok)
	if code != http.StatusOK {
		t.Fatalf("admin freeze: got %d, want 200", code)
	}
	data, _ := body["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("admin freeze: missing data envelope (body=%v)", body)
	}
	if got := data["order"].(map[string]interface{})["status"].(string); got != string(StatusLocked) {
		t.Fatalf("freeze status = %v, want locked", got)
	}
}
