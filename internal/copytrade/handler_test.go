package copytrade

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

var testVerifier = middleware.NewTokenVerifier("test-secret")

// newTestServiceForHandler 构造用于 handler 测试的 Service（内存存储 + mock 执行器）。
func newTestServiceForHandler() (*Service, *MemStore) {
	store := NewMemStore()
	mock := &mockExec{}
	lg := ledger.New()
	// seed follower 2 的 copytrade 账本余额，供平台复制费结算。
	_ = lg.ReceiveOnChain(2, "USDT", settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")), "seed")
	svc := NewService(store, lg, mock, Config{MinNotional: 1, CopyFeeRate: 0.001}, nil)
	return svc, store
}

func setupRouter(s *Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	s.RegisterRoutes(r, testVerifier)
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
	code, _ := resp["code"].(float64)
	return int(code)
}

// --- 未鉴权 401 ---

func TestHandlerUnauthorized_CreateLead(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/leads", strings.NewReader(`{"name":"alice"}`))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerUnauthorized_ListLeads(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/leads", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerUnauthorized_Follow(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows", strings.NewReader(`{"lead_id":1}`))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerUnauthorized_MyFollows(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/follows", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerUnauthorized_AdminLeads(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/leads", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerUnauthorized_SimulateTrade(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/admin/simulate-trade", strings.NewReader(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

// --- 管理员权限 403 ---

func TestHandlerNonAdmin_Forbidden_SimulateTrade(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	body := `{"symbol":"BTC_USDT","price":100,"qty":1,"taker_side":"buy","taker_id":1,"maker_id":99,"ts":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/admin/simulate-trade", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1)) // 普通用户
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerNonAdmin_Forbidden_AdminLeads(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/leads", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d", w.Code)
	}
}

func TestHandlerNonAdmin_Forbidden_AdminFollows(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/follows", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d", w.Code)
	}
}

func TestHandlerNonAdmin_Forbidden_AdminCopies(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/copies", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d", w.Code)
	}
}

func TestHandlerNonAdmin_Forbidden_Reconcile(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/reconcile", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d", w.Code)
	}
}

// --- 创建带单高手 ---

func TestHandlerCreateLead_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	body := `{"name":"alice","bio":"top trader"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/leads", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["name"] != "alice" {
		t.Fatalf("name: want alice, got %v", data["name"])
	}
	if data["status"] != string(LeadActive) {
		t.Fatalf("status: want active, got %v", data["status"])
	}
}

func TestHandlerCreateLead_InvalidBody(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/leads", strings.NewReader(`{invalid`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

// --- 列出活跃带单高手 ---

func TestHandlerListLeads_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	// 先创建一个 lead
	body := `{"name":"bob","bio":""}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/leads", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(10))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("create lead: %d", w.Code)
	}

	// 列出 leads
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/v1/copytrade/leads", nil)
	req2.Header.Set("Authorization", authHeader(2))
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expect 200, got %d", w2.Code)
	}
	data := respData(t, w2)
	leads, ok := data["leads"].([]interface{})
	if !ok {
		t.Fatalf("leads not array: %v", data["leads"])
	}
	if len(leads) != 1 {
		t.Fatalf("expect 1 lead, got %d", len(leads))
	}
}

// --- 关闭带单 ---

func TestHandlerCloseLead_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	// 创建 lead（uid=5）
	_, _ = svc.CreateLead(5, "carol", "")

	// 关闭（uid=5，本人）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/leads/5/close", nil)
	req.Header.Set("Authorization", authHeader(5))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["status"] != string(LeadClosed) {
		t.Fatalf("status: want closed, got %v", data["status"])
	}
}

func TestHandlerCloseLead_Forbidden(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(5, "carol", "")

	// uid=6 关闭别人的 lead → 400（ErrNotOwner 经 handler 返回）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/leads/5/close", nil)
	req.Header.Set("Authorization", authHeader(6))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 (not owner), got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerCloseLead_InvalidID(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/leads/abc/close", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

// --- 关注跟单 ---

func TestHandlerFollow_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(10, "lead10", "")

	body := `{"lead_id":10,"copy_ratio":0.5,"allocated_amount":1000,"follower_token":"tok-f2"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["lead_id"] != float64(10) {
		t.Fatalf("lead_id: want 10, got %v", data["lead_id"])
	}
	if data["follower_id"] != float64(20) {
		t.Fatalf("follower_id: want 20, got %v", data["follower_id"])
	}
	if data["status"] != string(FollowActive) {
		t.Fatalf("status: want active, got %v", data["status"])
	}
}

func TestHandlerFollow_MissingBody(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows", strings.NewReader(`{`))
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

func TestHandlerFollow_MissingLeadID(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	body := `{"copy_ratio":0.5,"follower_token":"tok"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerFollow_MissingFollowerToken(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(10, "lead10", "")

	body := `{"lead_id":10,"copy_ratio":0.5}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerFollow_LeadNotFound(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	body := `{"lead_id":9999,"copy_ratio":0.5,"follower_token":"tok"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerFollow_DuplicateFollow(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(10, "lead10", "")
	body := `{"lead_id":10,"copy_ratio":0.5,"follower_token":"tok"}`

	// 第一次关注
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("first follow: %d", w.Code)
	}

	// 第二次重复关注
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/copytrade/follows", strings.NewReader(body))
	req2.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w2, req2)
	if w2.Code != 400 {
		t.Fatalf("expect 400 (already following), got %d body=%s", w2.Code, w2.Body.String())
	}
}

// --- 我的关注列表 ---

func TestHandlerMyFollows_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(10, "lead10", "")
	_, _ = svc.RegisterFollow(20, 10, 0.5, 0, "tok")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/follows", nil)
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	follows, ok := data["follows"].([]interface{})
	if !ok {
		t.Fatalf("follows not array: %v", data["follows"])
	}
	if len(follows) != 1 {
		t.Fatalf("expect 1 follow, got %d", len(follows))
	}
}

// --- 停止关注 ---

func TestHandlerStopFollow_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(10, "lead10", "")
	f, _ := svc.RegisterFollow(20, 10, 0.5, 0, "tok")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows/"+itoa(f.ID)+"/stop", nil)
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["status"] != string(FollowStopped) {
		t.Fatalf("status: want stopped, got %v", data["status"])
	}
}

func TestHandlerStopFollow_Forbidden(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(10, "lead10", "")
	f, _ := svc.RegisterFollow(20, 10, 0.5, 0, "tok")

	// uid=30 尝试停止 uid=20 的关注
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows/"+itoa(f.ID)+"/stop", nil)
	req.Header.Set("Authorization", authHeader(30))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 (not owner), got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerStopFollow_InvalidID(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/follows/abc/stop", nil)
	req.Header.Set("Authorization", authHeader(20))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

// --- 管理员模拟成交 ---

func TestHandlerAdminSimulateTrade_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	// 创建 lead 和 follow，触发复制
	_, _ = svc.CreateLead(1, "alice", "")
	_, _ = svc.RegisterFollow(2, 1, 0.5, 0, "tok-f2")

	body := `{"symbol":"BTC_USDT","price":100,"qty":1,"taker_side":"buy","taker_id":1,"maker_id":99,"ts":1000}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/admin/simulate-trade", strings.NewReader(body))
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["status"] != "replicated" {
		t.Fatalf("status: want replicated, got %v", data["status"])
	}
}

func TestHandlerAdminSimulateTrade_InvalidBody(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/admin/simulate-trade", strings.NewReader(`{invalid`))
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

func TestHandlerAdminSimulateTrade_MissingFields(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	body := `{"symbol":"BTC_USDT"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/copytrade/admin/simulate-trade", strings.NewReader(body))
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// --- 管理员列表接口 ---

func TestHandlerAdminLeads_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(1, "alice", "")
	_, _ = svc.CreateLead(2, "bob", "")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/leads", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	leads, ok := data["leads"].([]interface{})
	if !ok {
		t.Fatalf("leads not array: %v", data["leads"])
	}
	if len(leads) != 2 {
		t.Fatalf("expect 2 leads, got %d", len(leads))
	}
}

func TestHandlerAdminFollows_Success(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	_, _ = svc.CreateLead(1, "alice", "")
	_, _ = svc.RegisterFollow(10, 1, 0.5, 0, "tok")
	_, _ = svc.RegisterFollow(20, 1, 1.0, 0, "tok2")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/follows", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	follows, ok := data["follows"].([]interface{})
	if !ok {
		t.Fatalf("follows not array: %v", data["follows"])
	}
	if len(follows) != 2 {
		t.Fatalf("expect 2 follows, got %d", len(follows))
	}
}

func TestHandlerAdminCopies_Empty(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/copies", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	copies, ok := data["copies"].([]interface{})
	if !ok {
		t.Fatalf("copies not array: %v", data["copies"])
	}
	if len(copies) != 0 {
		t.Fatalf("expect 0 copies, got %d", len(copies))
	}
}

// --- 管理员对账 ---

func TestHandlerAdminReconcile_Balanced(t *testing.T) {
	svc, _ := newTestServiceForHandler()
	r := setupRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/copytrade/admin/reconcile", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d", w.Code)
	}
	data := respData(t, w)
	if data["balanced"] != true {
		t.Fatalf("expect balanced=true, got %v", data["balanced"])
	}
}

// itoa 简易 int64 → 字符串转换（避免引入 strconv 测试文件中）。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	b := make([]byte, 0, 20)
	if n < 0 {
		b = append(b, '-')
		n = -n
	}
	start := len(b)
	for n > 0 {
		b = append(b, byte('0'+n%10))
		n /= 10
	}
	// reverse digits
	for i, j := start, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
