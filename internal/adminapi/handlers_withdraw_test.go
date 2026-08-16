package adminapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func writeEnvelope(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gin.H{"code": 0, "data": data, "message": ""})
}

// newAdminWithFutures 构造 admin 路由，并把 futures 上游指向 fake 服务，返回 admin token。
func newAdminWithFutures(t *testing.T, futuresURL string) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "admin123"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"futures": futuresURL}
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
	r, tok := newAdminWithFutures(t, fake.URL)

	ws := getWithdrawals(t, r, tok)
	if len(ws) != 3 {
		t.Fatalf("expected 3 withdrawals, got %d", len(ws))
	}
	// 已 finalized 的 h2 应映射为 approved；其余 pending。
	byID := map[int64]adminapi.Withdrawal{}
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
	r, tok := newAdminWithFutures(t, fake.URL)

	ws := getWithdrawals(t, r, tok)
	var h1 int64
	for _, w := range ws {
		if w.TxHash == "h1" {
			h1 = w.ID
		}
	}
	if h1 == 0 {
		t.Fatal("h1 not found in list")
	}
	// approve h1
	req := httptest.NewRequest(http.MethodPost, "/api/admin/withdrawals/"+itoa(h1)+"/approve", nil)
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

// TestAdminWithdrawalRejectCallsFutures 验证 reject 真正调用 futures 拒绝端点。
func TestAdminWithdrawalRejectCallsFutures(t *testing.T) {
	st := &fakeFuturesState{}
	fake := newFakeFutures(t, st)
	r, tok := newAdminWithFutures(t, fake.URL)

	ws := getWithdrawals(t, r, tok)
	var h3 int64
	for _, w := range ws {
		if w.TxHash == "h3" {
			h3 = w.ID
		}
	}
	if h3 == 0 {
		t.Fatal("h3 not found in list")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/withdrawals/"+itoa(h3)+"/reject", nil)
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
	r, tok := newAdminWithFutures(t, fake.URL)

	ws := getWithdrawals(t, r, tok)
	var h1 int64
	for _, w := range ws {
		if w.TxHash == "h1" {
			h1 = w.ID
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/withdrawals/"+itoa(h1)+"/approve", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("approve with failing upstream should be 502, got %d %s", w.Code, w.Body.String())
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// TestAdminUsersBalanceEnriched 验证用户列表的 Balance 从 futures 钱包余额真实填充（不再恒为 0）。
// 注意：listUsers 在 user 服务不可达时降级为内存示例用户（alice=1001 等），
// 余额仍由 futures 余额端点 enrich。
func TestAdminUsersBalanceEnriched(t *testing.T) {
	st := &fakeFuturesState{}
	fake := newFakeFutures(t, st)
	r, tok := newAdminWithFutures(t, fake.URL)

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
	// 找到 alice（id=1001），其余额应被 enrich 为 125000.5。
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
