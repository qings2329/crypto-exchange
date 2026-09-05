package staking

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

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
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

func newHandlerTestService() *Service {
	store := NewMemStore()
	l := ledger.New()
	return NewService(store, l, NewMockBackend(), Config{AccrueInterval: 0}, zap.NewNop())
}

// createTestProduct 通过 store 直接创建一个测试产品，返回 ID。
func createTestProduct(t *testing.T, svc *Service) int64 {
	t.Helper()
	p := &StakingProduct{
		Name:         "ETH Staking",
		Chain:        "eth",
		Validator:    "0xval",
		ContractAddr: "0xcontract",
		Asset:        "ETH",
		AnnualRate:   0.05,
		DurationDays: 0,
		MinAmount:    settlement.AssetAmountFromFloat(0.1, 8),
		Status:       ProductActive,
	}
	if err := svc.store.CreateProduct(p); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return p.ID
}

// seedUser 给用户注入指定资产的可用余额。
func seedUser(t *testing.T, svc *Service, uid int64, asset string, amount float64) {
	t.Helper()
	dec := settlement.AssetDecimalsByName(asset)
	amt := settlement.AssetAmountFromFloat(amount, dec)
	if err := svc.ledger.Transfer(ledger.SysChainClearing, uid, asset, amt, "seed", fmt.Sprintf("seed:%d:%s", uid, asset)); err != nil {
		t.Fatalf("seed user %d: %v", uid, err)
	}
}

// ---- list products ----

func TestHandlerListProductsUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/products", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerListProductsEmpty(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/products", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	products, ok := data["products"].([]interface{})
	if !ok {
		t.Fatalf("products not array: %v", data)
	}
	if len(products) != 0 {
		t.Fatalf("expect 0 products, got %d", len(products))
	}
}

func TestHandlerListProductsWithSeed(t *testing.T) {
	svc := newHandlerTestService()
	createTestProduct(t, svc)
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/products", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	products := data["products"].([]interface{})
	if len(products) != 1 {
		t.Fatalf("expect 1 product, got %d", len(products))
	}
}

// ---- create product (admin) ----

func TestHandlerCreateProductUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	body := `{"name":"X","chain":"eth","asset":"ETH","annual_rate":0.05,"min_amount":0.1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/products", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403 (non-admin), got %d", w.Code)
	}
}

func TestHandlerCreateProductNoToken(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	body := `{"name":"X","chain":"eth","asset":"ETH","annual_rate":0.05}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/products", strings.NewReader(body))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerCreateProductAdmin(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	body := `{"name":"ETH Stake","chain":"eth","validator":"0xval","contract_addr":"0xc","asset":"ETH","annual_rate":0.05,"duration_days":30,"min_amount":0.1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/products", strings.NewReader(body))
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["name"] != "ETH Stake" {
		t.Fatalf("expect name ETH Stake, got %v", data["name"])
	}
}

func TestHandlerCreateProductInvalidBody(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/products", strings.NewReader("not-json"))
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

// ---- subscribe ----

func TestHandlerSubscribeUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	body := `{"product_id":1,"amount":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/subscribe", strings.NewReader(body))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerSubscribeHappyPath(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	r := setupRouter(svc)

	body := fmt.Sprintf(`{"product_id":%d,"amount":1}`, pid)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/subscribe", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["user_id"] != float64(1) {
		t.Fatalf("expect user_id=1, got %v", data["user_id"])
	}
	if data["status"] != "active" {
		t.Fatalf("expect status active, got %v", data["status"])
	}
	// 本金应被锁定到 SysStaking
	bal, _, _ := svc.ledger.Balance(ledger.SysStaking, "ETH")
	one := settlement.AssetAmountFromFloat(1, 8)
	if bal.Cmp(one) != 0 {
		t.Fatalf("SysStaking ETH balance=%v, want %v", bal, one)
	}
}

func TestHandlerSubscribeInvalidBody(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/subscribe", strings.NewReader("bad"))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

func TestHandlerSubscribeZeroAmount(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	r := setupRouter(svc)

	body := fmt.Sprintf(`{"product_id":%d,"amount":0}`, pid)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/subscribe", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerSubscribeBelowMin(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	r := setupRouter(svc)

	body := fmt.Sprintf(`{"product_id":%d,"amount":0.01}`, pid)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/subscribe", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerSubscribeNonexistentProduct(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	body := `{"product_id":999,"amount":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/subscribe", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// ---- holdings (my delegations) ----

func TestHandlerMyDelegationsUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/holdings", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerMyDelegationsEmpty(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/holdings", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	delegs := data["delegations"].([]interface{})
	if len(delegs) != 0 {
		t.Fatalf("expect 0 delegations, got %d", len(delegs))
	}
}

func TestHandlerMyDelegationsFiltered(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	seedUser(t, svc, 2, "ETH", 100)
	// user1 质押
	dec := settlement.AssetDecimalsByName("ETH")
	amt := settlement.AssetAmountFromFloat(1, dec)
	_, _ = svc.Subscribe(1, pid, amt)
	_, _ = svc.Subscribe(2, pid, amt)

	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/holdings", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	delegs := data["delegations"].([]interface{})
	if len(delegs) != 1 {
		t.Fatalf("expect 1 delegation for user1, got %d", len(delegs))
	}
}

// ---- unbond ----

func TestHandlerUnbondUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	body := `{"delegation_id":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/unbond", strings.NewReader(body))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerUnbondHappyPath(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	d, _ := svc.Subscribe(1, pid, settlement.AssetAmountFromFloat(1, 8))

	r := setupRouter(svc)
	body := fmt.Sprintf(`{"delegation_id":%d}`, d.ID)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/unbond", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["status"] != "unbonding" {
		t.Fatalf("expect status unbonding, got %v", data["status"])
	}
}

func TestHandlerUnbondInvalidBody(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/unbond", strings.NewReader("bad"))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

func TestHandlerUnbondZeroDelegationID(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/unbond", strings.NewReader(`{"delegation_id":0}`))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

func TestHandlerUnbondNotOwner(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	seedUser(t, svc, 2, "ETH", 100)
	d, _ := svc.Subscribe(1, pid, settlement.AssetAmountFromFloat(1, 8))

	r := setupRouter(svc)
	body := fmt.Sprintf(`{"delegation_id":%d}`, d.ID)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/unbond", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(2))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 (not owner), got %d body=%s", w.Code, w.Body.String())
	}
}

// ---- release ----

func TestHandlerReleaseUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	body := `{"delegation_id":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/release", strings.NewReader(body))
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerReleaseHappyPath(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	d, _ := svc.Subscribe(1, pid, settlement.AssetAmountFromFloat(1, 8))
	_, _ = svc.Unbond(1, d.ID)

	r := setupRouter(svc)
	body := fmt.Sprintf(`{"delegation_id":%d}`, d.ID)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/release", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["status"] != "unbonded" {
		t.Fatalf("expect status unbonded, got %v", data["status"])
	}
	// 本金应释放回 SysStaking → 0
	bal, _, _ := svc.ledger.Balance(ledger.SysStaking, "ETH")
	if bal.Sign() != 0 {
		t.Fatalf("SysStaking ETH balance should be 0 after release, got %v", bal)
	}
}

func TestHandlerReleaseNotOwner(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	d, _ := svc.Subscribe(1, pid, settlement.AssetAmountFromFloat(1, 8))
	_, _ = svc.Unbond(1, d.ID)

	r := setupRouter(svc)
	body := fmt.Sprintf(`{"delegation_id":%d}`, d.ID)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/release", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(2))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400 (not owner), got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandlerReleaseInvalidBody(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/release", strings.NewReader("bad"))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

// ---- admin accrue ----

func TestHandlerAccrueUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/admin/accrue", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerAccrueNonAdmin(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/admin/accrue", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d", w.Code)
	}
}

func TestHandlerAccrueAdmin(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	_, _ = svc.Subscribe(1, pid, settlement.AssetAmountFromFloat(1, 8))

	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/admin/accrue", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["accrued"] == nil {
		t.Fatalf("expect accrued field, got %v", data)
	}
}

// ---- admin holdings ----

func TestHandlerAdminDelegationsUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/admin/holdings", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerAdminDelegationsNonAdmin(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/admin/holdings", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d", w.Code)
	}
}

func TestHandlerAdminDelegations(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	seedUser(t, svc, 2, "ETH", 100)
	_, _ = svc.Subscribe(1, pid, settlement.AssetAmountFromFloat(1, 8))
	_, _ = svc.Subscribe(2, pid, settlement.AssetAmountFromFloat(2, 8))

	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/admin/holdings", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	delegs := data["delegations"].([]interface{})
	if len(delegs) != 2 {
		t.Fatalf("expect 2 delegations, got %d", len(delegs))
	}
}

// ---- admin reconcile ----

func TestHandlerReconcileUnauthorized(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/admin/reconcile", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

func TestHandlerReconcileNonAdmin(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/admin/reconcile", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d", w.Code)
	}
}

func TestHandlerReconcileBalanced(t *testing.T) {
	svc := newHandlerTestService()
	r := setupRouter(svc)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/staking/admin/reconcile", nil)
	req.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if data["balanced"] != true {
		t.Fatalf("expect balanced=true (empty state), got %v", data["balanced"])
	}
}

// ---- subscribe + unbond + release 完整流程 ----

func TestHandlerFullLifecycle(t *testing.T) {
	svc := newHandlerTestService()
	pid := createTestProduct(t, svc)
	seedUser(t, svc, 1, "ETH", 100)
	r := setupRouter(svc)

	// subscribe
	subBody := fmt.Sprintf(`{"product_id":%d,"amount":5}`, pid)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/staking/subscribe", strings.NewReader(subBody))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("subscribe: expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	subData := respData(t, w)
	delID := int64(subData["id"].(float64))

	// holdings 应有 1 条
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/staking/holdings", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	hData := respData(t, w)
	if len(hData["delegations"].([]interface{})) != 1 {
		t.Fatalf("expect 1 delegation in holdings, got %d", len(hData["delegations"].([]interface{})))
	}

	// unbond
	unbondBody := fmt.Sprintf(`{"delegation_id":%d}`, delID)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/staking/unbond", strings.NewReader(unbondBody))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("unbond: expect 200, got %d body=%s", w.Code, w.Body.String())
	}

	// release
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/v1/staking/release", strings.NewReader(unbondBody))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("release: expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	relData := respData(t, w)
	if relData["status"] != "unbonded" {
		t.Fatalf("expect unbonded after release, got %v", relData["status"])
	}

	// SysStaking 应归零
	bal, _, _ := svc.ledger.Balance(ledger.SysStaking, "ETH")
	if bal.Sign() != 0 {
		t.Fatalf("SysStaking ETH=%v, want 0 after full lifecycle", bal)
	}
}
