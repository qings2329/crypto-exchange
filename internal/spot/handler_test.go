package spot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/client"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

var testVerifier = middleware.NewTokenVerifier("test-secret")

// fakeMatcher 是 matcherClient 的测试假实现，记录 Submit/Cancel 调用次数，
// 并在 Submit 时回写 o.ID（与 client.Client 行为一致），供断言幂等/冒充。
type fakeMatcher struct {
	submitCalls int
	cancelCalls int
	orders      map[int64]matching.OrderView
	trades      []matching.TradeView
	depthBids   []matching.Level
	depthAsks   []matching.Level
	depthFail   bool
	mu          sync.Mutex
}

var fakeOID int64

func (f *fakeMatcher) Submit(symbol string, o *matching.Order) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitCalls++
	if o.ID == 0 {
		o.ID = atomic.AddInt64(&fakeOID, 1)
	}
	if f.orders == nil {
		f.orders = make(map[int64]matching.OrderView)
	}
	f.orders[o.ID] = matching.OrderView{ID: o.ID, UserID: o.UserID, Symbol: symbol, Market: "spot", Side: sideStr(o.Side), Price: o.Price, Qty: o.Qty}
	return true
}

func (f *fakeMatcher) CancelOrder(symbol string, orderID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
	_, ok := f.orders[orderID]
	delete(f.orders, orderID)
	return ok, nil
}

func (f *fakeMatcher) GetOrder(orderID int64) (matching.OrderView, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.orders[orderID]
	return v, ok
}

func (f *fakeMatcher) ListOrders(userID int64, symbol, status string, limit int) []matching.OrderView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]matching.OrderView, 0, len(f.orders))
	for _, v := range f.orders {
		if v.UserID == userID {
			out = append(out, v)
		}
	}
	return out
}

func (f *fakeMatcher) ListTrades(userID int64, symbol string, limit int) []matching.TradeView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]matching.TradeView, 0, len(f.trades))
	for _, v := range f.trades {
		if v.TakerID == userID || v.MakerID == userID {
			out = append(out, v)
		}
	}
	return out
}

func (f *fakeMatcher) Depth(symbol string) (bids, asks []matching.Level, ok bool) {
	return f.depthBids, f.depthAsks, !f.depthFail
}

func (f *fakeMatcher) Watch(ctx context.Context, symbols []string, onTrade func(client.TradeEvent), onDepth func(client.DepthEvent)) error {
	return nil
}

func sideStr(s matching.Side) string {
	if s == matching.Sell {
		return "sell"
	}
	return "buy"
}

func setupRouter(s *Server) *gin.Engine {
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

// countSpotTrade 统计某 ref 下「成交转账」的笔数（而非流水条数）。
// 每笔 ledger.Transfer 写入 2 条单边流水（借/贷），故一次完整结算（计价腿+基础腿）
// 计为 2 笔转账；发生双付时该值会翻倍（4、6…），据此检测重放/并发双付。
func countSpotTrade(s *Server, ref string) int {
	n := 0
	for _, e := range s.ledgerSvc.Log() {
		if e.BizType == "spot_trade" && e.Ref == ref && e.Delta.Sign() < 0 {
			n++ // 仅计借方单边，一笔记账=一笔转账
		}
	}
	return n
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

func extractOrderID(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	data := respData(t, w)
	v, ok := data["order_id"]
	if !ok {
		t.Fatalf("no order_id in resp %s", w.Body.String())
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	t.Fatalf("bad order_id type %T", v)
	return 0
}

// 用例1（F1 重放双付）：同 Trade 连调 settleFill 两次，spot_trade 流水数恒为 2。
func TestSettleFillNoDoublePay_Replay(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0)
	seed(s, 2, 0, 10)
	buyRec, _ := s.reserveOnOpen(1, matching.Buy, 100, 1, "BTC_USDT")
	sellRec, _ := s.reserveOnOpen(2, matching.Sell, 100, 1, "BTC_USDT")
	s.openOrders[101] = buyRec
	s.openOrders[202] = sellRec

	trade := matching.Trade{Price: 100, Qty: 1, TakerSide: matching.Buy, TakerID: 1, MakerID: 2, TakerOID: 101, MakerOID: 202}
	ref := tradeRef("BTC_USDT", trade)

	if err := s.settleFill("BTC_USDT", trade); err != nil {
		t.Fatalf("first settle failed: %v", err)
	}
	if got := countSpotTrade(s, ref); got != 2 {
		t.Fatalf("after first settle expect 2 entries, got %d", got)
	}
	if err := s.settleFill("BTC_USDT", trade); err != nil {
		t.Fatalf("replay settle failed: %v", err)
	}
	if got := countSpotTrade(s, ref); got != 2 {
		t.Fatalf("after replay expect still 2 entries, got %d", got)
	}
	// 余额与单次结算一致（无双付）。
	if b, _, _ := s.ledgerSvc.Balance(1, "BTC"); !eqAmt(b, 1, "BTC") {
		t.Fatalf("buyer BTC=%v want 1", b)
	}
	if u, _, _ := s.ledgerSvc.Balance(2, "USDT"); !eqAmt(u, 100, "USDT") {
		t.Fatalf("seller USDT=%v want 100", u)
	}
}

// 用例2（F1 竞态 + 重放）：并发同 Trade 调用 settleFill，仍仅 2 条流水、无 data race。
func TestSettleFillConcurrentReplay(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0)
	seed(s, 2, 0, 10)
	buyRec, _ := s.reserveOnOpen(1, matching.Buy, 100, 1, "BTC_USDT")
	sellRec, _ := s.reserveOnOpen(2, matching.Sell, 100, 1, "BTC_USDT")
	s.openOrders[101] = buyRec
	s.openOrders[202] = sellRec

	trade := matching.Trade{Price: 100, Qty: 1, TakerSide: matching.Buy, TakerID: 1, MakerID: 2, TakerOID: 101, MakerOID: 202}
	ref := tradeRef("BTC_USDT", trade)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.settleFill("BTC_USDT", trade)
		}()
	}
	wg.Wait()
	if got := countSpotTrade(s, ref); got != 2 {
		t.Fatalf("expect 2 entries after concurrent replay, got %d", got)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatal("ledger unbalanced after concurrent settle")
	}
}

// 用例3（F1 下单幂等）：同 client_oid 两次下单，假 matcher 的 Submit 仅 1 次、冻结不翻倍。
func TestReserveIdempotentByClientOID(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 100000, 0)
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","side":"buy","price":100,"qty":1,"client_oid":"abc"}`
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/api/v1/spot/order", strings.NewReader(body))
		req.Header.Set("Authorization", authHeader(1))
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("order %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	if fm.submitCalls != 1 {
		t.Fatalf("expect Submit called once, got %d", fm.submitCalls)
	}
	if _, f, _ := s.ledgerSvc.Balance(1, "USDT"); !eqAmt(f, 100, "USDT") {
		t.Fatalf("frozen should be 100 (not doubled), got %v", f)
	}
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/spot/order", strings.NewReader(body))
	req2.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w2, req2)
	data := respData(t, w2)
	if data["idempotent"] != true {
		t.Fatalf("second order should be idempotent, body=%s", w2.Body.String())
	}
}

// 用例4（F4 身份伪造）：token uid=1 但 body 带 user_id=2，订单须以 uid=1 下单。
func TestHandleOrderRejectsImpersonation(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 100000, 0)
	seed(s, 2, 0, 10)
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","side":"buy","price":100,"qty":1,"user_id":2}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/spot/order", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	oid := extractOrderID(t, w)
	s.freezeMu.Lock()
	rec := s.openOrders[oid]
	s.freezeMu.Unlock()
	if rec == nil {
		t.Fatal("no open order recorded")
	}
	if rec.user != 1 {
		t.Fatalf("impersonation succeeded: order booked under uid=%d, want 1", rec.user)
	}
}

// 用例5b（F4 正常撤单）：uid=1 撤本人订单 → 200，冻结释放、转发撮合并清理本地记录。
func TestHandleCancelOwnOrderSuccess(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 100000, 0)
	rec, _ := s.reserveOnOpen(1, matching.Buy, 100, 1, "BTC_USDT") // 冻结 100 USDT
	s.openOrders[555] = rec
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","order_id":555}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/spot/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	if fm.cancelCalls != 1 {
		t.Fatalf("expect cancel forwarded once, got %d", fm.cancelCalls)
	}
	s.freezeMu.Lock()
	still := s.openOrders[555]
	s.freezeMu.Unlock()
	if still != nil {
		t.Fatal("own order rec must be removed after cancel")
	}
	if _, f, _ := s.ledgerSvc.Balance(1, "USDT"); !eqAmt(f, 0, "USDT") {
		t.Fatalf("frozen must be released, got %v", f)
	}
}

// 用例5c（F4 回退核验-本人）：本地无记录但撮合引擎归属本人 → 撤单成功并转发。
func TestHandleCancelGetOrderFallbackOwned(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 100000, 0)
	// 本地 openOrders 无 123，但撮合引擎记为其归属 uid=1。
	fm.orders = make(map[int64]matching.OrderView)
	fm.orders[123] = matching.OrderView{ID: 123, UserID: 1, Symbol: "BTC_USDT", Market: "spot"}
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","order_id":123}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/spot/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	if fm.cancelCalls != 1 {
		t.Fatalf("expect cancel forwarded once, got %d", fm.cancelCalls)
	}
}

// 用例5d（F4 回退核验-越权）：本地无记录但撮合引擎归属他人 → 403，且不转发撤单。
func TestHandleCancelGetOrderFallbackForbidden(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 100000, 0)
	seed(s, 2, 0, 10)
	fm.orders = make(map[int64]matching.OrderView)
	fm.orders[123] = matching.OrderView{ID: 123, UserID: 2, Symbol: "BTC_USDT", Market: "spot"} // 属 uid=2
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","order_id":123}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/spot/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1)) // 攻击者 uid=1
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d body=%s", w.Code, w.Body.String())
	}
	if fm.cancelCalls != 0 {
		t.Fatalf("must not forward cancel, got %d", fm.cancelCalls)
	}
}

// 用例3b（F5 余额不足）：可用余额不足以冻结 → 400，不提交、不冻结。
func TestHandleOrderInsufficientBalance(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 50, 0) // 仅 50 USDT，买 100 USDT 不足
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","side":"buy","price":100,"qty":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/spot/order", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
	if fm.submitCalls != 0 {
		t.Fatalf("must not submit, got %d", fm.submitCalls)
	}
	if _, f, _ := s.ledgerSvc.Balance(1, "USDT"); !eqAmt(f, 0, "USDT") {
		t.Fatalf("must not freeze, got %v", f)
	}
}

// 用例3c（F2 sell 方向预冻结）：卖单冻结基础资产（BTC），提交撮合。
func TestHandleOrderSellFreezesBase(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 0, 10) // 持有 10 BTC
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","side":"sell","price":100,"qty":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/spot/order", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	if fm.submitCalls != 1 {
		t.Fatalf("expect submit once, got %d", fm.submitCalls)
	}
	if _, f, _ := s.ledgerSvc.Balance(1, "BTC"); !eqAmt(f, 1, "BTC") {
		t.Fatalf("expect 1 BTC frozen, got %v", f)
	}
}

// 用例5（F4 越权撤单）：uid=1 撤 uid=2 的订单 → 403，且不释放冻结、不转发撤单。
func TestHandleCancelForbiddenForOtherUser(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 100000, 0)
	seed(s, 2, 100000, 0)
	// 预冻结属于 uid=2 的订单 555。
	rec, _ := s.reserveOnOpen(2, matching.Buy, 100, 1, "BTC_USDT")
	s.openOrders[555] = rec
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","order_id":555}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/spot/cancel", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1)) // 攻击者 uid=1
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d body=%s", w.Code, w.Body.String())
	}
	if fm.cancelCalls != 0 {
		t.Fatalf("must not forward cancel to matching, got %d", fm.cancelCalls)
	}
	s.freezeMu.Lock()
	still := s.openOrders[555]
	s.freezeMu.Unlock()
	if still == nil {
		t.Fatal("victim's order rec must remain")
	}
	if _, f, _ := s.ledgerSvc.Balance(2, "USDT"); !eqAmt(f, 100, "USDT") {
		t.Fatalf("victim's frozen changed=%v, want 100", f)
	}
}

// 用例6（F5 零价）：price<=0 下单 → 400，不冻结、不提交。
func TestHandleOrderRejectsZeroPrice(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	seed(s, 1, 100000, 0)
	r := setupRouter(s)

	body := `{"symbol":"BTC_USDT","side":"buy","price":0,"qty":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/spot/order", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d body=%s", w.Code, w.Body.String())
	}
	if fm.submitCalls != 0 {
		t.Fatalf("must not submit, got %d", fm.submitCalls)
	}
	if _, f, _ := s.ledgerSvc.Balance(1, "USDT"); !eqAmt(f, 0, "USDT") {
		t.Fatalf("must not freeze, got %v", f)
	}
}

// 用例7（F5 纵深：零价白嫖）：cost==0 的 Trade 须被 settleFill 拒绝，卖方 base 不被无偿划转。
func TestSettleFillZeroPriceNoFreeLunch(t *testing.T) {
	s := newTestServer()
	seed(s, 2, 0, 10) // 卖方有 BTC
	trade := matching.Trade{Price: 0, Qty: 1, TakerSide: matching.Buy, TakerID: 1, MakerID: 2, TakerOID: 101, MakerOID: 202}
	if err := s.settleFill("BTC_USDT", trade); err == nil {
		t.Fatal("expect error for zero-price trade")
	}
	if b, _, _ := s.ledgerSvc.Balance(2, "BTC"); !eqAmt(b, 10, "BTC") {
		t.Fatalf("seller BTC must be unchanged (no free lunch), got %v", b)
	}
	if got := countSpotTrade(s, tradeRef("BTC_USDT", trade)); got != 0 {
		t.Fatalf("expect 0 spot_trade entries, got %d", got)
	}
}

// 用例8（F2 部分成交钳位）：2 BTC 单分两次 1 BTC 成交，末笔精确结算，撤单后无残留冻结。
func TestPartialFillClampNoResidual(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0)
	seed(s, 2, 0, 10)
	rec, _ := s.reserveOnOpen(1, matching.Buy, 100, 2, "BTC_USDT") // 冻结 200 USDT
	s.openOrders[888] = rec

	t1 := matching.Trade{Price: 100, Qty: 1, TakerSide: matching.Buy, TakerID: 1, MakerID: 2, TakerOID: 888, MakerOID: 999}
	t2 := matching.Trade{Price: 100, Qty: 1, TakerSide: matching.Buy, TakerID: 1, MakerID: 2, TakerOID: 888, MakerOID: 998}
	if err := s.settleFill("BTC_USDT", t1); err != nil {
		t.Fatalf("t1 settle: %v", err)
	}
	if _, f, _ := s.ledgerSvc.Balance(1, "USDT"); !eqAmt(f, 100, "USDT") {
		t.Fatalf("after 1st fill frozen=%v want 100", f)
	}
	if err := s.settleFill("BTC_USDT", t2); err != nil {
		t.Fatalf("t2 settle: %v", err)
	}
	s.freezeMu.Lock()
	n := len(s.openOrders)
	s.freezeMu.Unlock()
	if n != 0 {
		t.Fatalf("expect openOrders empty after full fill, got %d", n)
	}
	if _, f, _ := s.ledgerSvc.Balance(1, "USDT"); !eqAmt(f, 0, "USDT") {
		t.Fatalf("residual frozen=%v want 0", f)
	}
}

// 用例9（F3 对账端点）：非 admin → 403；admin → 200 且 balanced 与账本一致。
func TestReconcileAdminEndpoint(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0)
	rec, _ := s.reserveOnOpen(1, matching.Buy, 100, 1, "BTC_USDT")
	s.openOrders[777] = rec
	r := setupRouter(s)

	// 非 admin
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/admin/reconcile", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403 for non-admin, got %d", w.Code)
	}

	// admin
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/v1/spot/admin/reconcile", nil)
	req2.Header.Set("Authorization", adminHeader())
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expect 200 for admin, got %d body=%s", w2.Code, w2.Body.String())
	}
	data := respData(t, w2)
	if data["balanced"] != true {
		t.Fatalf("expect balanced=true, got %v", data["balanced"])
	}
}

// 用例10（F1 撤单 vs 在途成交）：并发撤单与结算同订单，不双付、账本平衡。
func TestCancelVsInFlightFill(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0)
	seed(s, 2, 0, 10)
	rec, _ := s.reserveOnOpen(1, matching.Buy, 100, 1, "BTC_USDT")
	s.openOrders[888] = rec
	sellRec, _ := s.reserveOnOpen(2, matching.Sell, 100, 1, "BTC_USDT")
	s.openOrders[202] = sellRec

	trade := matching.Trade{Price: 100, Qty: 1, TakerSide: matching.Buy, TakerID: 1, MakerID: 2, TakerOID: 888, MakerOID: 202}
	ref := tradeRef("BTC_USDT", trade)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.settleFill("BTC_USDT", trade)
		}()
		go func() {
			defer wg.Done()
			s.freezeMu.Lock()
			if r, ok := s.openOrders[888]; ok {
				s.releaseRemaining(r)
				delete(s.openOrders, 888)
			}
			s.freezeMu.Unlock()
		}()
	}
	wg.Wait()
	if got := countSpotTrade(s, ref); got != 2 {
		t.Fatalf("expect exactly 2 spot_trade entries (no double pay), got %d", got)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatal("ledger unbalanced")
	}
}

// 用例11（用户订单列表）：仅返回本人 spot 订单，排除他人单与 futures 单；支持 margin/limit 过滤。
func TestHandleOrdersReturnsOwnOnly(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.orders = map[int64]matching.OrderView{
		1: {ID: 1, UserID: 1, Symbol: "BTC_USDT", Market: "spot"},
		2: {ID: 2, UserID: 2, Symbol: "BTC_USDT", Market: "spot"},     // 他人
		3: {ID: 3, UserID: 1, Symbol: "BTC_USDT", Market: "futures"},  // 非 spot
		4: {ID: 4, UserID: 1, Symbol: "BTC_USDT", Market: "spot", IsMargin: true},
	}
	r := setupRouter(s)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders", nil)
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
	if len(arr) != 2 {
		t.Fatalf("expect 2 own spot orders, got %d", len(arr))
	}
}

// 用例11b（margin 过滤）：仅返回杠杆单。
func TestHandleOrdersMarginFilter(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.orders = map[int64]matching.OrderView{
		1: {ID: 1, UserID: 1, Symbol: "BTC_USDT", Market: "spot"},
		4: {ID: 4, UserID: 1, Symbol: "BTC_USDT", Market: "spot", IsMargin: true},
	}
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders?margin=1", nil)
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
		t.Fatalf("expect 1 margin order, got %d", len(arr))
	}
}

// 用例11c（limit 分页）：limit=1 截断。
func TestHandleOrdersLimit(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.orders = map[int64]matching.OrderView{
		1: {ID: 1, UserID: 1, Symbol: "BTC_USDT", Market: "spot"},
		4: {ID: 4, UserID: 1, Symbol: "BTC_USDT", Market: "spot"},
	}
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders?limit=1", nil)
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
		t.Fatalf("expect 1 after limit, got %d", len(arr))
	}
}

// 用例11d（未鉴权）：无 token → 401。
func TestHandleOrdersUnauthorized(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

// 用例12（订单详情-本人）：返回本人订单。
func TestHandleOrderDetailOwn(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.orders = map[int64]matching.OrderView{10: {ID: 10, UserID: 1, Symbol: "BTC_USDT", Market: "spot"}}
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders/10", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	order, ok := data["order"].(map[string]interface{})
	if !ok {
		t.Fatalf("no order in resp: %v", data)
	}
	if int64(order["id"].(float64)) != 10 {
		t.Fatalf("expect order id 10, got %v", order["id"])
	}
}

// 用例12b（订单详情-越权）：他人订单 → 403。
func TestHandleOrderDetailForbidden(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.orders = map[int64]matching.OrderView{10: {ID: 10, UserID: 2, Symbol: "BTC_USDT", Market: "spot"}}
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders/10", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expect 403, got %d", w.Code)
	}
}

// 用例12c（订单详情-不存在）：404。
func TestHandleOrderDetailNotFound(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.orders = map[int64]matching.OrderView{}
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders/999", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expect 404, got %d", w.Code)
	}
}

// 用例12d（订单详情-非法 id）：400。
func TestHandleOrderDetailBadID(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders/abc", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}

// 用例12e（订单详情-未鉴权）：401。
func TestHandleOrderDetailUnauthorized(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/orders/10", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expect 401, got %d", w.Code)
	}
}

// 用例13（成交列表-本人）：仅返回本人 spot 成交。
func TestHandleTradesReturnsOwn(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.trades = []matching.TradeView{
		{ID: 1, Symbol: "BTC_USDT", Market: "spot", TakerID: 1},
		{ID: 2, Symbol: "BTC_USDT", Market: "spot", TakerID: 2},          // 他人
		{ID: 3, Symbol: "BTC_USDT", Market: "futures", TakerID: 1},       // 非 spot
	}
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/trades", nil)
	req.Header.Set("Authorization", authHeader(1))
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	arr, ok := data["trades"].([]interface{})
	if !ok {
		t.Fatalf("trades not array: %v", data["trades"])
	}
	if len(arr) != 1 {
		t.Fatalf("expect 1 own spot trade, got %d", len(arr))
	}
}

// 用例14（行情深度-成功）：返回聚合后的 bids/asks。
func TestHandleDepthSuccess(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.depthBids = []matching.Level{{Price: 100, Orders: []*matching.Order{{Qty: 1}}}}
	fm.depthAsks = []matching.Level{{Price: 200, Orders: []*matching.Order{{Qty: 2}}}}
	r := setupRouter(s) // /depth 为公开端点（豁免鉴权）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/depth?symbol=BTC_USDT", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expect 200, got %d body=%s", w.Code, w.Body.String())
	}
	data := respData(t, w)
	if _, ok := data["bids"]; !ok {
		t.Fatal("no bids in resp")
	}
	if _, ok := data["asks"]; !ok {
		t.Fatal("no asks in resp")
	}
}

// 用例14b（行情深度-撮合不可用）：400。
func TestHandleDepthUnavailable(t *testing.T) {
	fm := &fakeMatcher{}
	s := newTestServer()
	s.client = fm
	fm.depthFail = true
	r := setupRouter(s)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spot/depth?symbol=BTC_USDT", nil)
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expect 400, got %d", w.Code)
	}
}
