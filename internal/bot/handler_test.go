package bot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

var testVerifier = middleware.NewTokenVerifier("test-secret")

func setupRouter(svc *Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc.RegisterRoutes(r, testVerifier)
	return r
}

func authHeader(uid int64) string {
	return "Bearer " + testVerifier.Issue(uid, time.Hour)
}

func adminHeader() string {
	return "Bearer " + testVerifier.IssueAdmin(99, "admin", nil, time.Hour)
}

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

func respCode(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	code, ok := resp["code"].(float64)
	if !ok {
		t.Fatalf("no code in resp %s", w.Body.String())
	}
	return int(code)
}

func seedStrategy(t *testing.T, svc *Service, uid int64) *BotStrategy {
	t.Helper()
	st := &BotStrategy{
		UserID:    uid,
		Name:      "test-grid",
		Market:    MarketSpot,
		Symbol:    "BTC_USDT",
		Side:      "buy",
		Type:      StrategyGrid,
		UserToken: "user-token-abc",
		Params: BotParams{
			GridLower:   90,
			GridUpper:   110,
			GridNum:     10,
			OrderAmount: 10,
		},
	}
	if err := svc.CreateStrategy(st); err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	return st
}

func newHandlerTestService() *Service {
	return NewService(NewMemStore(), NewMockPrice(), nil, Config{}, zap.NewNop())
}

// --- 未鉴权 ---

func TestHandlerUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	endpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/api/v1/bot/strategies"},
		{"GET", "/api/v1/bot/strategies"},
		{"POST", "/api/v1/bot/strategies/1/start"},
		{"POST", "/api/v1/bot/strategies/1/stop"},
		{"GET", "/api/v1/bot/strategies/1/orders"},
		{"GET", "/api/v1/bot/admin/strategies"},
		{"POST", "/api/v1/bot/admin/strategies/1/tick"},
	}
	for _, ep := range endpoints {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(ep.method, ep.path, nil)
		r.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatalf("%s %s: expect 401, got %d", ep.method, ep.path, w.Code)
		}
	}
}

// --- 创建策略 ---

func TestHandlerCreateStrategy(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	body := `{"name":"my-grid","market":"spot","symbol":"BTC_USDT","side":"buy","type":"grid","user_token":"tok123","params":{"grid_lower":90,"grid_upper":110,"grid_num":5,"order_amount":10}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["name"] != "my-grid" {
		t.Fatalf("expect name=my-grid, got %v", data["name"])
	}
	if data["status"] != string(StrategyStopped) {
		t.Fatalf("expect status=stopped, got %v", data["status"])
	}
	if data["user_id"].(float64) != 1 {
		t.Fatalf("expect user_id=1, got %v", data["user_id"])
	}
}

func TestHandlerCreateStrategyNoUserToken(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	body := `{"name":"my-grid","market":"spot","symbol":"BTC_USDT","side":"buy","type":"grid","params":{"grid_lower":90,"grid_upper":110,"order_amount":10}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerCreateStrategyInvalidBody(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies", strings.NewReader("not-json"))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerCreateStrategyInvalidParam(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	// grid 但 lower >= upper
	body := `{"name":"bad","market":"spot","symbol":"BTC_USDT","side":"buy","type":"grid","user_token":"tok","params":{"grid_lower":200,"grid_upper":100,"order_amount":10}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 for invalid grid params, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerCreateStrategyBadMarket(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	body := `{"name":"bad","market":"unknown","symbol":"BTC_USDT","side":"buy","type":"grid","user_token":"tok","params":{"grid_lower":90,"grid_upper":110,"order_amount":10}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerCreateDCA(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	body := `{"name":"dca","market":"futures","symbol":"ETH_USDT","side":"buy","type":"dca","user_token":"tok","params":{"dca_interval_sec":3600,"dca_amount":50,"order_amount":50}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["type"] != "dca" {
		t.Fatalf("expect type=dca, got %v", data["type"])
	}
}

func TestHandlerCreateMA(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	body := `{"name":"ma","market":"spot","symbol":"BTC_USDT","side":"sell","type":"ma","user_token":"tok","params":{"ma_short":7,"ma_long":25,"order_amount":20}}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["type"] != "ma" {
		t.Fatalf("expect type=ma, got %v", data["type"])
	}
}

// --- 我的策略列表 ---

func TestHandlerMyStrategies(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	seedStrategy(t, svc, 1)
	seedStrategy(t, svc, 1)
	seedStrategy(t, svc, 2) // 其他用户

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/bot/strategies", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	arr, ok := data["strategies"].([]interface{})
	if !ok {
		t.Fatalf("strategies not array: %v", data["strategies"])
	}
	if len(arr) != 2 {
		t.Fatalf("expect 2 own strategies, got %d", len(arr))
	}
}

func TestHandlerMyStrategiesEmpty(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/bot/strategies", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	arr := data["strategies"].([]interface{})
	if len(arr) != 0 {
		t.Fatalf("expect 0 strategies, got %d", len(arr))
	}
}

// --- 启动/停止策略 ---

func TestHandlerStartStrategy(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/bot/strategies/%d/start", st.ID), nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["status"] != string(StrategyActive) {
		t.Fatalf("expect status=active, got %v", data["status"])
	}
}

func TestHandlerStopStrategy(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1)
	_ = svc.StartStrategy(1, st.ID) // 先启动

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/bot/strategies/%d/stop", st.ID), nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["status"] != string(StrategyStopped) {
		t.Fatalf("expect status=stopped, got %v", data["status"])
	}
}

func TestHandlerStartStrategyNotFound(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies/999/start", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerStartStrategyBadID(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/strategies/abc/start", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerStartStrategyForbidden(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1) // 属于 uid=1

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/bot/strategies/%d/start", st.ID), nil)
	req.Header.Set("Authorization", authHeader(2)) // uid=2 尝试启动
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 for not-owner, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerStopStrategyForbidden(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/bot/strategies/%d/stop", st.ID), nil)
	req.Header.Set("Authorization", authHeader(2))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 for not-owner, got %d body=%s", w.Code, w.Body.String())
	}
}

// --- 订单列表 ---

func TestHandlerListOrders(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1)

	// 手动插入订单到 store
	_ = svc.store.CreateOrder(&BotOrder{
		StrategyID: st.ID, UserID: 1, Market: MarketSpot, Symbol: "BTC_USDT",
		Side: "buy", Price: 100, Qty: 0.1, Status: "submitted", CreatedAt: time.Now().Unix(),
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/bot/strategies/%d/orders", st.ID), nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	arr, ok := data["orders"].([]interface{})
	if !ok {
		t.Fatalf("orders not array: %v", data["orders"])
	}
	if len(arr) != 1 {
		t.Fatalf("expect 1 order, got %d", len(arr))
	}
}

func TestHandlerListOrdersForbidden(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1) // 属于 uid=1

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/bot/strategies/%d/orders", st.ID), nil)
	req.Header.Set("Authorization", authHeader(2)) // uid=2 查看
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerListOrdersNotFound(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/bot/strategies/999/orders", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerListOrdersBadID(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/bot/strategies/abc/orders", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// --- 管理端点 ---

func TestHandlerAdminStrategies(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	seedStrategy(t, svc, 1)
	seedStrategy(t, svc, 2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/bot/admin/strategies", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	arr, ok := data["strategies"].([]interface{})
	if !ok {
		t.Fatalf("strategies not array: %v", data["strategies"])
	}
	if len(arr) != 2 {
		t.Fatalf("expect 2 strategies, got %d", len(arr))
	}
}

func TestHandlerAdminStrategiesForbidden(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/bot/admin/strategies", nil)
	req.Header.Set("Authorization", authHeader(1)) // 普通用户
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerAdminTick(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1)
	_ = svc.StartStrategy(1, st.ID) // 必须 active 才能 tick

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/bot/admin/strategies/%d/tick", st.ID), nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	// grid tick 初始化挂单时 executor 返回错误（空 URL），但 grid 引擎对单笔失败容错（log+continue），
	// 因此整体 tick 仍成功返回200。
	if w.Code != 200 {
		t.Fatalf("expect 200 (grid tick is fault-tolerant), got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerAdminTickStoppedStrategy(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1) // 默认 stopped

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/bot/admin/strategies/%d/tick", st.ID), nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 for stopped strategy, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerAdminTickBadID(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/admin/strategies/abc/tick", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerAdminTickNotFound(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/bot/admin/strategies/999/tick", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerAdminTickForbidden(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	st := seedStrategy(t, svc, 1)
	_ = svc.StartStrategy(1, st.ID)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/bot/admin/strategies/%d/tick", st.ID), nil)
	req.Header.Set("Authorization", authHeader(1)) // 普通用户
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d body=%s", w.Code, w.Body.String())
	}
}
