package adminapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

// fakeFuturesState 记录 admin 真正调用了哪些 futures 审批端点，用于验证审核落地。
type fakeFuturesState struct {
	approved    []string
	rejected    []string
	failApprove bool
}

// newFakeFutures 起一个模拟 futures 的 httptest 服务，实现提币 hold 队列与审批/拒绝端点。
// 返回结构套 {code,data} 信封，供 admin 的 UpstreamClient 解包。
func newFakeFutures(t *testing.T, st *fakeFuturesState) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/futures/wallet/withdraw/holds", func(w http.ResponseWriter, r *http.Request) {
		holds := []map[string]interface{}{
			{"id": "h1", "user_id": 1, "asset": "USDT", "amount": 100, "fee": 1, "chain": "Ethereum", "address": "0xabc",
				"created_at": "2026-08-16T00:00:00Z", "hold_until": "2026-08-16T00:00:30Z", "finalized": false, "cancelled": false},
			{"id": "h2", "user_id": 2, "asset": "BTC", "amount": 0.5, "fee": 0.0001, "chain": "Bitcoin", "address": "bc1xyz",
				"created_at": "2026-08-16T00:00:00Z", "hold_until": "2026-08-16T00:00:30Z", "finalized": true, "cancelled": false},
			{"id": "h3", "user_id": 3, "asset": "ETH", "amount": 2, "fee": 0.01, "chain": "Ethereum", "address": "0xdef",
				"created_at": "2026-08-16T00:00:00Z", "hold_until": "2026-08-16T00:00:30Z", "finalized": false, "cancelled": false},
		}
		writeEnvelope(w, gin.H{"holds": holds})
	})
	mux.HandleFunc("/api/v1/futures/wallet/balance", func(w http.ResponseWriter, r *http.Request) {
		uid := r.URL.Query().Get("user_id")
		var avail float64
		switch uid {
		case "1001":
			avail = 125000.5
		case "1002":
			avail = 3400
		default:
			avail = 0
		}
		writeEnvelope(w, gin.H{"user_id": uid, "asset": "USDT", "available": avail, "exists": true})
	})
	mux.HandleFunc("/api/v1/futures/wallet/withdraw/approve/", func(w http.ResponseWriter, r *http.Request) {
		if st.failApprove {
			w.WriteHeader(http.StatusInternalServerError)
			writeEnvelope(w, gin.H{"error": "boom"})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/futures/wallet/withdraw/approve/")
		st.approved = append(st.approved, id)
		writeEnvelope(w, gin.H{"status": "approved", "hold_id": id})
	})
	mux.HandleFunc("/api/v1/futures/wallet/withdraw/reject/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/futures/wallet/withdraw/reject/")
		st.rejected = append(st.rejected, id)
		writeEnvelope(w, gin.H{"status": "rejected", "hold_id": id})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newFakeUser 起一个模拟 user 服务的 httptest 服务，实现管理员用户列表端点，
// 供 listUsers 成功路径（经 futures 余额端点 enrich 余额）测试使用。
func newFakeUser(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.ServeMux{}
	mux.HandleFunc("/api/v1/user/admin/list", func(w http.ResponseWriter, r *http.Request) {
		users := []map[string]interface{}{
			{"id": 1001, "username": "alice", "email": "alice@x.com", "phone": "", "status": 0, "kyc_level": 2, "created_at": "2026-08-16T00:00:00Z"},
		}
		writeEnvelope(w, gin.H{"users": users})
	})
	srv := httptest.NewServer(&mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeEnvelope(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gin.H{"code": 0, "data": data, "message": ""})
}

// newAdminWithFutures 构造 admin 路由，并把 futures 上游指向 fake 服务，返回 admin token。
// userURL 非空时同时把 user 上游指向 fake 服务（用于走 listUsers 成功路径）。
func newAdminWithFutures(t *testing.T, futuresURL, userURL string) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "admin123"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"futures": futuresURL}
	if userURL != "" {
		cfg.Services["user"] = userURL
	}
	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	// 登录拿 admin token（super_admin 自动含 withdraw:approval 权限）。
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

// getWithdrawals 拉取管理后台提现列表，返回解析后的 Withdrawal 切片。
func getWithdrawals(t *testing.T, r *gin.Engine, tok string) []adminapi.Withdrawal {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/withdrawals", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list withdrawals failed: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data []adminapi.Withdrawal `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env.Data
}

func TestAdminWithdrawalListMapsHoldID(t *testing.T) {
	st := &fakeFuturesState{}
	fake := newFakeFutures(t, st)
	r, tok := newAdminWithFutures(t, fake.URL, "")

	ws := getWithdrawals(t, r, tok)
	if len(ws) != 3 {
		t.Fatalf("expected 3 withdrawals, got %d", len(ws))
	}
	// 已 finalized 的 h2 应映射为 approved；其余 pending。
	byID := map[string]adminapi.Withdrawal{}
	for _, w := range ws {
		byID[w.ID] = w
	}
	// 找到 h1（pending）与 h2（approved by finalized）。
	var h1, h2 adminapi.Withdrawal
	found1, found2 := false, false
	for _, w := range ws {
		if w.TxHash == "h1" {
			h1 = w
			found1 = true
		}
		if w.TxHash == "h2" {
			h2 = w
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Fatalf("expected holds h1/h2 present: %+v", ws)
	}
	if h2.Status != "approved" {
		t.Fatalf("finalized hold h2 should map to approved, got %s", h2.Status)
	}
	if h1.Status != "pending" {
		t.Fatalf("pending hold h1 should map to pending, got %s", h1.Status)
	}
	_ = h1
}

// TestAdminWithdrawalApproveCallsFutures 验证 approve 真正调用 futures 审批端点并回显状态。
func TestAdminWithdrawalApproveCallsFutures(t *testing.T) {
	st := &fakeFuturesState{}
	fake := newFakeFutures(t, st)
	r, tok := newAdminWithFutures(t, fake.URL, "")

	ws := getWithdrawals(t, r, tok)
	var h1 string
	for _, w := range ws {
		if w.TxHash == "h1" {
			h1 = w.ID
		}
	}
	if h1 == "" {
		t.Fatal("h1 not found in list")
	}
	// approve h1
	req := httptest.NewRequest(http.MethodPost, "/api/admin/withdrawals/"+h1+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve should succeed, got %d %s", w.Code, w.Body.String())
	}
	if len(st.approved) != 1 || st.approved[0] != "h1" {
		t.Fatalf("futures approve endpoint should be called with h1, got %+v", st.approved)
	}
	// 再次列表应回显 approved。
	ws2 := getWithdrawals(t, r, tok)
	for _, w := range ws2 {
		if w.TxHash == "h1" && w.Status != "approved" {
			t.Fatalf("after approve, h1 should show approved, got %s", w.Status)
		}
	}
}

// TestAdminWithdrawalApproveIdempotent 验证重复审批被终态短路（发现 1）：
// 第二次直接幂等返回 already:true，且不重复调用 futures 审批端点。
func TestAdminWithdrawalApproveIdempotent(t *testing.T) {
	st := &fakeFuturesState{}
	fake := newFakeFutures(t, st)
	r, tok := newAdminWithFutures(t, fake.URL, "")

	ws := getWithdrawals(t, r, tok)
	var h1 string
	for _, w := range ws {
		if w.TxHash == "h1" {
			h1 = w.ID
		}
	}
	if h1 == "" {
		t.Fatal("h1 not found in list")
	}
	approve := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/withdrawals/"+h1+"/approve", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := approve(); w.Code != http.StatusOK {
		t.Fatalf("first approve should succeed, got %d %s", w.Code, w.Body.String())
	}
	w2 := approve()
	if w2.Code != http.StatusOK {
		t.Fatalf("second approve should succeed (idempotent), got %d %s", w2.Code, w2.Body.String())
	}
	var resp2 struct {
		Data struct {
			Already bool `json:"already"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if !resp2.Data.Already {
		t.Fatalf("second approve should be idempotent (already:true), got %s", w2.Body.String())
	}
	if len(st.approved) != 1 {
		t.Fatalf("futures approve should be called exactly once, got %+v", st.approved)
	}
}

// TestAdminWithdrawalRejectCallsFutures 验证 reject 真正调用 futures 拒绝端点。
func TestAdminWithdrawalRejectCallsFutures(t *testing.T) {
	st := &fakeFuturesState{}
	fake := newFakeFutures(t, st)
	r, tok := newAdminWithFutures(t, fake.URL, "")

	ws := getWithdrawals(t, r, tok)
	var h3 string
	for _, w := range ws {
		if w.TxHash == "h3" {
			h3 = w.ID
		}
	}
	if h3 == "" {
		t.Fatal("h3 not found in list")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/withdrawals/"+h3+"/reject", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reject should succeed, got %d %s", w.Code, w.Body.String())
	}
	if len(st.rejected) != 1 || st.rejected[0] != "h3" {
		t.Fatalf("futures reject endpoint should be called with h3, got %+v", st.rejected)
	}
}

// TestAdminWithdrawalApproveUpstreamFailure 验证 futures 不可达/失败时 approve 返回 502。
func TestAdminWithdrawalApproveUpstreamFailure(t *testing.T) {
	st := &fakeFuturesState{failApprove: true}
	fake := newFakeFutures(t, st)
	r, tok := newAdminWithFutures(t, fake.URL, "")

	ws := getWithdrawals(t, r, tok)
	var h1 string
	for _, w := range ws {
		if w.TxHash == "h1" {
			h1 = w.ID
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/withdrawals/"+h1+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("approve with failing upstream should be 502, got %d %s", w.Code, w.Body.String())
	}
}

// TestAdminUsersBalanceEnriched 验证用户列表的 Balance 从 futures 钱包余额真实填充（不再恒为 0）。
// 走 listUsers 成功路径（user 服务可达 + futures 余额端点 enrich）。
func TestAdminUsersBalanceEnriched(t *testing.T) {
	st := &fakeFuturesState{}
	fake := newFakeFutures(t, st)
	userFake := newFakeUser(t)
	r, tok := newAdminWithFutures(t, fake.URL, userFake.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list users failed: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data []adminapi.AdminUser `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data) == 0 {
		t.Fatal("expected at least one user")
	}
	// 找到 alice（id=1001），其余额应被 futures 余额端点 enrich 为 125000.5。
	var alice *adminapi.AdminUser
	for i := range env.Data {
		if env.Data[i].ID == 1001 {
			alice = &env.Data[i]
		}
	}
	if alice == nil {
		t.Fatalf("alice (id=1001) not found in users: %+v", env.Data)
	}
	if alice.Balance != 125000.5 {
		t.Fatalf("alice balance should be enriched to 125000.5, got %v", alice.Balance)
	}
}

// TestAdminCashflowListDegraded 验证上游 futures 不可达时，充值/提现列表返回与正常路径同构的
// 空数组（data 始终为数组，不返回伪造记录，发现 4），并经 X-Degraded 响应头告知前端上游不可用。
// 注意：data 在降级与正常路径下均为数组，避免前端因 object/array 形态切换而解析失败。
func TestAdminCashflowListDegraded(t *testing.T) {
	st := &fakeFuturesState{}
	fake := newFakeFutures(t, st)
	r, tok := newAdminWithFutures(t, fake.URL, "")
	fake.Close() // 模拟 futures 不可达

	for _, path := range []string{"/api/admin/deposits", "/api/admin/withdrawals"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s degraded list should still 200, got %d %s", path, w.Code, w.Body.String())
		}
		// X-Degraded 响应头必须存在，告知前端上游不可用。
		if got := w.Header().Get("X-Degraded"); got != "futures-unavailable" {
			t.Fatalf("%s expected X-Degraded: futures-unavailable header, got %q", path, got)
		}
		// data 必须是空数组（与正常路径同构），而非伪造记录。
		var env struct {
			Data []interface{} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s body should decode as data array, got %s", path, w.Body.String())
		}
		if len(env.Data) != 0 {
			t.Fatalf("%s expected empty data array (no fake records), got %d: %s", path, len(env.Data), w.Body.String())
		}
	}
}

// TestAdminUsersListDegraded 验证上游 user 服务不可达时，用户列表返回与正常路径同构的空数组
// （data 始终为数组，不返回伪造的示例用户，发现 4 对称项），并经 X-Degraded 响应头告知前端。
func TestAdminUsersListDegraded(t *testing.T) {
	fake := newFakeFutures(t, &fakeFuturesState{})
	// 仅配置 futures，不配置 user → 走 user 降级分支。
	r, tok := newAdminWithFutures(t, fake.URL, "")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("users degraded list should still 200, got %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Degraded"); got != "user-unavailable" {
		t.Fatalf("expected X-Degraded: user-unavailable header, got %q", got)
	}
	var env struct {
		Data []interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("users body should decode as data array, got %s", w.Body.String())
	}
	if len(env.Data) != 0 {
		t.Fatalf("expected empty users data array (no fake users), got %d: %s", len(env.Data), w.Body.String())
	}
}
