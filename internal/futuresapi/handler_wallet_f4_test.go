package futuresapi

import (
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

// newF4Server 构造完整路由（全局 Auth + 钱包路由），用于验证端点级角色守卫（F4）。
// 账户 1 充值 10000 USDT，冷却期 30s、地址验证 0、日限额 50000，覆盖链上提现/审批场景。
func newF4Server(t *testing.T) (*Server, *gin.Engine, *middleware.TokenVerifier) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	l := ledger.New()
	if err := l.Deposit(1, "USDT", 10000, "seed"); err != nil {
		t.Fatal(err)
	}
	l.SetWithdrawHoldPeriod(30 * time.Second)
	l.SetAddressVerifyPeriod(0)
	l.SetDailyWithdrawLimit("USDT", 50000)
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

	holdID, _, err := s.ledgerSvc.RequestWithdrawHold(1, "USDT", 100, 1, "Ethereum", "0xabc")
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

// TestWalletFeeBadAmount 验证 /wallet/fee 对非法 amount 返回 400 而非静默当 0（F5a 修复）。
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
