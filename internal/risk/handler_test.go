package risk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

var testVerifier = middleware.NewTokenVerifier("test-secret")

func setupRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.Auth(testVerifier))
	h.RegisterRoutes(r)
	return r
}

func authHeader(uid int64) string {
	return "Bearer " + testVerifier.Issue(uid, time.Hour)
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

// ---------- 鉴权 ----------

// 所有路由无 token → 401
func TestHandlerUnauthorized(t *testing.T) {
	h := NewHandler(New(NewMemStore()))
	r := setupRouter(h)

	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/risk/rules"},
		{"POST", "/api/v1/risk/rules"},
		{"GET", "/api/v1/risk/blacklist"},
		{"POST", "/api/v1/risk/blacklist"},
		{"DELETE", "/api/v1/risk/blacklist?target=x"},
		{"GET", "/api/v1/risk/blacklist/check?target=x"},
		{"POST", "/api/v1/risk/check/withdraw"},
		{"POST", "/api/v1/risk/check/order"},
		{"GET", "/api/v1/risk/events"},
	}
	for _, rt := range routes {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(rt.method, rt.path, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expect 401, got %d", rt.method, rt.path, w.Code)
		}
	}
}

// ---------- Rules ----------

func TestHandlerAddRule(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	body := `{"kind":"withdraw_limit","name":"BTC daily","max_amount_per_day":"100","min_kyc_level":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/rules", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["kind"] != "withdraw_limit" {
		t.Fatalf("kind= %v", data["kind"])
	}
}

func TestHandlerAddRuleMissingKind(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	body := `{"name":"test"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/rules", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerAddRuleInvalidBody(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/rules", strings.NewReader("not json"))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerListRules(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddRule(&RiskRule{Kind: "withdraw_limit", Name: "r1"})
	svc.AddRule(&RiskRule{Kind: "order_limit", Name: "r2"})
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/rules", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if int(data["count"].(float64)) != 2 {
		t.Fatalf("count= %v", data["count"])
	}
}

func TestHandlerListRulesByKind(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddRule(&RiskRule{Kind: "withdraw_limit", Name: "r1"})
	svc.AddRule(&RiskRule{Kind: "order_limit", Name: "r2"})
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/rules?kind=withdraw_limit", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if int(data["count"].(float64)) != 1 {
		t.Fatalf("count= %v", data["count"])
	}
}

// ---------- Blacklist ----------

func TestHandlerAddBlacklist(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	body := `{"target":"user123","kind":"user","reason":"fraud"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/blacklist", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["target"] != "user123" {
		t.Fatalf("target= %v", data["target"])
	}
}

func TestHandlerAddBlacklistMissingKind(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	body := `{"target":"user123"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/blacklist", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerAddBlacklistInvalidBody(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/blacklist", strings.NewReader("bad"))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerRemoveBlacklist(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddBlacklist("user1", "user", "fraud")
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/risk/blacklist?target=user1", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	ok, _ := svc.IsBlacklisted("user1")
	if ok {
		t.Fatal("user1 should be removed")
	}
}

func TestHandlerRemoveBlacklistMissingTarget(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/risk/blacklist", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerRemoveBlacklistNotFound(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/risk/blacklist?target=nonexistent", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expect 404, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerListBlacklist(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddBlacklist("user1", "user", "fraud")
	svc.AddBlacklist("addr1", "address", "sanctioned")
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/blacklist", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if int(data["count"].(float64)) != 2 {
		t.Fatalf("count= %v", data["count"])
	}
}

func TestHandlerListBlacklistByKind(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddBlacklist("user1", "user", "fraud")
	svc.AddBlacklist("addr1", "address", "sanctioned")
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/blacklist?kind=user", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if int(data["count"].(float64)) != 1 {
		t.Fatalf("count= %v", data["count"])
	}
}

// ---------- Check blacklist ----------

func TestHandlerCheckBlacklistHit(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddBlacklist("user1", "user", "fraud")
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/blacklist/check?target=user1", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["blacklisted"] != true {
		t.Fatalf("blacklisted= %v", data["blacklisted"])
	}
}

func TestHandlerCheckBlacklistMiss(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/blacklist/check?target=clean", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["blacklisted"] != false {
		t.Fatalf("blacklisted= %v", data["blacklisted"])
	}
}

func TestHandlerCheckBlacklistMissingTarget(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/blacklist/check", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// ---------- Check withdraw ----------

func TestHandlerCheckWithdrawAllowed(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","amount":0.5,"kyc_level":1,"address":"bc1qok"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/withdraw", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != true {
		t.Fatalf("allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckWithdrawUserBlacklisted(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddBlacklist("1", "user", "fraud")
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","amount":0.5,"kyc_level":2,"address":"bc1qok"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/withdraw", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != false {
		t.Fatalf("expect denied, allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckWithdrawAddressBlacklisted(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddBlacklist("bc1qfraud", "address", "sanctioned")
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","amount":0.5,"kyc_level":2,"address":"bc1qfraud"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/withdraw", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != false {
		t.Fatalf("expect denied, allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckWithdrawKycTooLow(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddRule(&RiskRule{Kind: "withdraw_limit", Name: "btc", MinKYCLevel: 3})
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","amount":0.5,"kyc_level":1,"address":"bc1qok"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/withdraw", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != false {
		t.Fatalf("expect denied, allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckWithdrawExceedsLimit(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddRule(&RiskRule{
		Kind:            "withdraw_limit",
		Name:            "btc",
		MinKYCLevel:     1,
		MaxAmountPerDay: settlement.AssetAmountFromInt64(1, 8), // 1 BTC
	})
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","amount":2,"kyc_level":1,"address":"bc1qok"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/withdraw", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != false {
		t.Fatalf("expect denied, allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckWithdrawInvalidBody(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/withdraw", strings.NewReader("bad"))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// ---------- Check order ----------

func TestHandlerCheckOrderAllowed(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","qty":0.1,"kyc_level":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/order", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != true {
		t.Fatalf("allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckOrderBlacklisted(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddBlacklist("1", "user", "fraud")
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","qty":0.1,"kyc_level":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/order", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != false {
		t.Fatalf("expect denied, allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckOrderKycTooLow(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddRule(&RiskRule{Kind: "order_limit", Name: "btc", MinKYCLevel: 2})
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","qty":0.1,"kyc_level":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/order", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != false {
		t.Fatalf("expect denied, allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckOrderExceedsLimit(t *testing.T) {
	svc := New(NewMemStore())
	svc.AddRule(&RiskRule{
		Kind:            "order_limit",
		Name:            "btc",
		MinKYCLevel:     1,
		MaxAmountPerDay: settlement.AssetAmountFromInt64(1, 8), // 1 BTC
	})
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","qty":5,"kyc_level":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/order", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"] != false {
		t.Fatalf("expect denied, allowed= %v", data["allowed"])
	}
}

func TestHandlerCheckOrderInvalidBody(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/order", strings.NewReader("bad"))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// ---------- Events ----------

func TestHandlerListEvents(t *testing.T) {
	svc := New(NewMemStore())
	svc.record(1, "withdraw_limit", "event1")
	svc.record(1, "order_limit", "event2")
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/events", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if int(data["count"].(float64)) != 2 {
		t.Fatalf("count= %v", data["count"])
	}
}

func TestHandlerListEventsByUserID(t *testing.T) {
	svc := New(NewMemStore())
	svc.record(1, "withdraw_limit", "u1")
	svc.record(2, "order_limit", "u2")
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/events?user_id=1", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if int(data["count"].(float64)) != 1 {
		t.Fatalf("count= %v", data["count"])
	}
}

func TestHandlerListEventsWithLimit(t *testing.T) {
	svc := New(NewMemStore())
	svc.record(1, "withdraw_limit", "e1")
	svc.record(1, "order_limit", "e2")
	svc.record(1, "withdraw_limit", "e3")
	r := setupRouter(NewHandler(svc))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/risk/events?limit=2", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if int(data["count"].(float64)) != 2 {
		t.Fatalf("count= %v", data["count"])
	}
}

// ---------- CheckPosition ----------

func TestHandlerCheckPositionUnauthorized(t *testing.T) {
	r := setupRouter(NewHandler(New(NewMemStore())))
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/position", strings.NewReader(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerCheckPositionAllowed(t *testing.T) {
	svc := New(NewMemStore())
	_, _ = svc.AddRule(&RiskRule{Kind: "position_limit", Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(10, 8)})
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","size":5,"kyc_level":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/position", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if !data["allowed"].(bool) {
		t.Fatalf("expected allowed, got %v", data)
	}
}

func TestHandlerCheckPositionExceedsLimit(t *testing.T) {
	svc := New(NewMemStore())
	_, _ = svc.AddRule(&RiskRule{Kind: "position_limit", Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(10, 8)})
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","size":15,"kyc_level":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/position", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"].(bool) {
		t.Fatalf("expected rejected, got allowed")
	}
}

func TestHandlerCheckPositionBlacklisted(t *testing.T) {
	svc := New(NewMemStore())
	_, _ = svc.AddBlacklist("1", "user", "test")
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","size":1,"kyc_level":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/position", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"].(bool) {
		t.Fatalf("blacklisted user should be rejected")
	}
}

func TestHandlerCheckPositionNoRule(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"asset":"BTC","size":99999,"kyc_level":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/position", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if !data["allowed"].(bool) {
		t.Fatalf("no rule should allow by default")
	}
}

func TestHandlerCheckPositionInvalidBody(t *testing.T) {
	r := setupRouter(NewHandler(New(NewMemStore())))
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/position", strings.NewReader(`{bad`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

// ---------- CheckFrequency ----------

func TestHandlerCheckFrequencyUnauthorized(t *testing.T) {
	r := setupRouter(NewHandler(New(NewMemStore())))
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/frequency", strings.NewReader(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerCheckFrequencyAllowed(t *testing.T) {
	svc := New(NewMemStore())
	_, _ = svc.AddRule(&RiskRule{Kind: "freq_limit", MaxCountPerDay: 5})
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"action":"login"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/frequency", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if !data["allowed"].(bool) {
		t.Fatalf("first call should be allowed")
	}
}

func TestHandlerCheckFrequencyExceedsLimit(t *testing.T) {
	svc := New(NewMemStore())
	_, _ = svc.AddRule(&RiskRule{Kind: "freq_limit", MaxCountPerDay: 2})
	r := setupRouter(NewHandler(svc))

	// 前两次应放行
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/api/v1/risk/check/frequency", strings.NewReader(`{"user_id":1,"action":"withdraw"}`))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
		data := respData(t, w)
		if !data["allowed"].(bool) {
			t.Fatalf("call %d: expected allowed", i+1)
		}
	}
	// 第三次应被拒
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/frequency", strings.NewReader(`{"user_id":1,"action":"withdraw"}`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	data := respData(t, w)
	if data["allowed"].(bool) {
		t.Fatalf("3rd call should be rejected")
	}
}

func TestHandlerCheckFrequencyBlacklisted(t *testing.T) {
	svc := New(NewMemStore())
	_, _ = svc.AddBlacklist("1", "user", "test")
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"action":"login"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/frequency", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["allowed"].(bool) {
		t.Fatalf("blacklisted user should be rejected")
	}
}

func TestHandlerCheckFrequencyNoRule(t *testing.T) {
	svc := New(NewMemStore())
	r := setupRouter(NewHandler(svc))

	body := `{"user_id":1,"action":"login"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/frequency", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if !data["allowed"].(bool) {
		t.Fatalf("no rule should allow by default")
	}
}

func TestHandlerCheckFrequencyInvalidBody(t *testing.T) {
	r := setupRouter(NewHandler(New(NewMemStore())))
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/risk/check/frequency", strings.NewReader(`{bad`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}
