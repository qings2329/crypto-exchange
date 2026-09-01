package futuresapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/risk"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// newF4Server 构造完整路由（全局 Auth + 钱包路由），用于验证端点级角色守卫（F4）。
// 账户 1 充值 10000 USDT，冷却期 30s、地址验证 0、日限额 50000，覆盖链上提现/审批场景。
func newF4Server(t *testing.T) (*Server, *gin.Engine, *middleware.TokenVerifier) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	l.SetWithdrawHoldPeriod(30 * time.Second)
	l.SetAddressVerifyPeriod(0)
	l.SetDailyWithdrawLimit("USDT", amt("USDT", 50000))
	if _, err := l.AddWithdrawAddress(1, "USDT", "Ethereum", "0xabc", "test"); err != nil {
		t.Fatal(err)
	}
	if err := l.ConfirmWithdrawAddress(1, "USDT", "Ethereum", "0xabc"); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		ledgerSvc:    l,
		chainWithdraw: settlement.NewMockWithdrawGateway(3, time.Second),
		feeModel:     settlement.NewFeeModel(),
	}
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	s.RegisterRoutes(r, verifier)
	return s, r, verifier
}

// TestWithdrawChainRequiresAdmin 验证 /withdraw/chain（绕过冷静期直接链上广播）仅管理员可调用，
// 普通用户必须被角色守卫拒绝（403），从而闭合 F4 冷静期绕过缺口。
func TestWithdrawChainRequiresAdmin(t *testing.T) {
	_, r, verifier := newF4Server(t)
	userTok := verifier.Issue(1, time.Hour)
	adminTok := verifier.IssueAdmin(1, middleware.RoleAdmin, nil, time.Hour)

	const body = `{"user_id":1,"asset":"USDT","chain":"Ethereum","amount":10,"address":"0xabc"}`
	call := func(tok string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/futures/wallet/withdraw/chain", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		r.ServeHTTP(w, req)
		return w
	}

	// 普通用户：必须被拒绝，不得绕过冷静期直接广播。
	if w := call(userTok); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin withdraw/chain should be 403, got %d: %s", w.Code, w.Body.String())
	}
	// 管理员：应放行（业务成功，非 401/403）。
	if w := call(adminTok); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("admin withdraw/chain should pass guard, got %d: %s", w.Code, w.Body.String())
	}
}

// TestWithdrawApproveRequiresAdmin 验证 /withdraw/approve（跳过冷静期强制放行）仅管理员可调用，
// 普通用户必须被角色守卫拒绝（403），与用户端 request→finalize 冷静期约束分离（F4）。
func TestWithdrawApproveRequiresAdmin(t *testing.T) {
	s, r, verifier := newF4Server(t)
	userTok := verifier.Issue(1, time.Hour)
	adminTok := verifier.IssueAdmin(1, middleware.RoleAdmin, nil, time.Hour)

	holdID, _, err := s.ledgerSvc.RequestWithdrawHold(1, "USDT", amt("USDT", 100), amt("USDT", 1), "Ethereum", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	call := func(tok string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/futures/wallet/withdraw/approve/"+holdID, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		r.ServeHTTP(w, req)
		return w
	}

	// 普通用户：必须被拒绝，不得跳过冷静期放行。
	if w := call(userTok); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin withdraw/approve should be 403, got %d: %s", w.Code, w.Body.String())
	}
	// 管理员：应放行（业务成功，非 401/403）。
	if w := call(adminTok); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("admin withdraw/approve should pass guard, got %d: %s", w.Code, w.Body.String())
	}
}

// TestWithdrawChainEnforcesRisk 验证管理员「代客直提」(/withdraw/chain) 不再绕过 risk 规则引擎
// （RISK-F3 修复）：冻结资金前须先经 risk.CheckWithdraw，对目标用户生效——命中地址黑名单直接 403。
func TestWithdrawChainEnforcesRisk(t *testing.T) {
	s, r, verifier := newF4Server(t)
	adminTok := verifier.IssueAdmin(1, middleware.RoleAdmin, nil, time.Hour)

	riskSvc := risk.New(risk.NewMemStore())
	if _, err := riskSvc.AddBlacklist("0xbad", "address", "sanctioned"); err != nil {
		t.Fatal(err)
	}
	s.riskSvc = riskSvc
	s.kycFetcherByID = func(userID int64) (int, error) { return 1, nil }

	call := func(addr string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"user_id":1,"asset":"USDT","chain":"Ethereum","amount":10,"address":%q}`, addr)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/futures/wallet/withdraw/chain", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminTok)
		r.ServeHTTP(w, req)
		return w
	}

	// 黑名单地址 → 403：代客直提不得绕过风控（此前静默放行即资金失窃面）。
	if w := call("0xbad"); w.Code != http.StatusForbidden {
		t.Fatalf("blacklisted address should be rejected 403, got %d: %s", w.Code, w.Body.String())
	}
	// 已确认白名单地址 0xabc → 通过风控，进入后续冻结/广播流程（非 403）。
	if w := call("0xabc"); w.Code == http.StatusForbidden {
		t.Fatalf("clean address should pass risk gate, got 403: %s", w.Body.String())
	}
}
// 该端点受全局 Auth 保护，故需携带合法 token 才能越过鉴权、到达参数校验。
func TestWalletFeeBadAmount(t *testing.T) {
	_, r, verifier := newF4Server(t)
	tok := verifier.Issue(1, time.Hour)

	call := func(amount string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet,
			"/api/v1/futures/wallet/fee?chain=Ethereum&asset=USDT&amount="+amount, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		r.ServeHTTP(w, req)
		return w
	}

	// 非数字 amount 必须被拒绝（400），不得返回误导性的 fee=0 估算。
	if w := call("abc"); w.Code != http.StatusBadRequest {
		t.Fatalf("fee with non-numeric amount should be 400, got %d: %s", w.Code, w.Body.String())
	}
	// 负数 amount 必须被拒绝（400）。
	if w := call("-5"); w.Code != http.StatusBadRequest {
		t.Fatalf("fee with negative amount should be 400, got %d: %s", w.Code, w.Body.String())
	}
	// 合法 amount 应正常估算（200）。
	if w := call("10"); w.Code != http.StatusOK {
		t.Fatalf("fee with valid amount should be 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDepositSelfUserFlow 契约：用户侧自助充值——uid 取 token（防冒充）、白名单、
// 上限/频控护栏，入账后返回可用/冻结余额。
func TestDepositSelfUserFlow(t *testing.T) {
	_, r, verifier := newF4Server(t)
	userTok := verifier.Issue(1, time.Hour)

	post := func(tok, body string) *httptest.ResponseRecorder {
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/futures/wallet/deposit/self", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 正常充值 500 USDT → ok，可用余额 10000+500。
	w := post(userTok, `{"asset":"USDT","amount":500}`)
	if w.Code != http.StatusOK {
		t.Fatalf("deposit self: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Status    string  `json:"status"`
			Asset     string  `json:"asset"`
			Available float64 `json:"available"`
			Frozen    float64 `json:"frozen"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.Status != "ok" || resp.Data.Asset != "USDT" || resp.Data.Available != 10500 {
		t.Fatalf("unexpected resp: %+v", resp.Data)
	}

	// 非白名单资产 → 400。
	if w = post(userTok, `{"asset":"DOGE","amount":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unsupported asset should 400, got %d", w.Code)
	}
	// 超上限 → 400。
	if w = post(userTok, `{"asset":"USDT","amount":10001}`); w.Code != http.StatusBadRequest {
		t.Fatalf("over-cap should 400, got %d", w.Code)
	}
	// 非法金额（负数）→ 400。
	if w = post(userTok, `{"asset":"USDT","amount":-5}`); w.Code != http.StatusBadRequest {
		t.Fatalf("negative should 400, got %d", w.Code)
	}

	// 冒充防护：body 里塞 user_id 无效（uid 一律取 token）。
	if w = post(userTok, `{"asset":"USDT","amount":10,"user_id":999}`); w.Code != http.StatusOK {
		t.Fatalf("self deposit should ignore body user_id, got %d %s", w.Code, w.Body.String())
	}
	var bal struct {
		Data struct {
			Available float64 `json:"available"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &bal)
	if bal.Data.Available != 10510 {
		t.Fatalf("expect credited to token uid (10510), got %+v", bal.Data)
	}

	// 频控：连续第 7 次（本窗口内已用 3 次）后触发 429 —— 用新 uid 隔离窗口精确验证。
	tok2 := verifier.Issue(2, time.Hour)
	for i := 0; i < selfDepositMaxPerMin; i++ {
		if w = post(tok2, `{"asset":"USDT","amount":1}`); w.Code != http.StatusOK {
			t.Fatalf("burst #%d should pass, got %d %s", i, w.Code, w.Body.String())
		}
	}
	if w = post(tok2, `{"asset":"USDT","amount":1}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit should 429, got %d", w.Code)
	}
}
