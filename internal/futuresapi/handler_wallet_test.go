package futuresapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/risk"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// amt 测试用：把人类单位浮点按资产标准小数位包装为 AssetAmount。
func amt(asset string, human float64) settlement.AssetAmount {
	return settlement.AssetAmountFromFloat(human, settlement.AssetDecimalsByName(asset))
}

// newWithdrawServer 构造最小 futures Server（仅钱包提现相关字段），用于提币审批测试。
// 账户 1 充值 10000 USDT，冷却期 30s、地址验证冷静期 0、每日限额 50000，覆盖 approve/reject 场景。
func newWithdrawServer(t *testing.T) *Server {
	t.Helper()
	l := ledger.New()
	if err := l.Deposit(1, "USDT", amt("USDT", 10000), "seed"); err != nil {
		t.Fatal(err)
	}
	l.SetWithdrawHoldPeriod(30 * time.Second)
	l.SetAddressVerifyPeriod(0) // 测试中确认地址后立即可用
	l.SetDailyWithdrawLimit("USDT", amt("USDT", 50000))
	if _, err := l.AddWithdrawAddress(1, "USDT", "Ethereum", "0xabc", "test"); err != nil {
		t.Fatal(err)
	}
	if err := l.ConfirmWithdrawAddress(1, "USDT", "Ethereum", "0xabc"); err != nil {
		t.Fatal(err)
	}
	gw := settlement.NewMockWithdrawGateway(3, time.Second)
	s := &Server{ledgerSvc: l, chainWithdraw: gw, riskSvc: risk.New(risk.NewMemStore())}
	// 多链/多资产手续费模型（与 NewServer 一致），否则 handleWithdrawRequest 估费时 nil panic。
	s.feeModel = settlement.NewFeeModel()
	s.feeModel.Register(settlement.ChainETH, "USDT", 0.1, 0)
	s.feeModel.Register(settlement.ChainETH, "ETH", 0.001, 0)
	s.feeModel.Register(settlement.ChainBTC, "BTC", 0.0005, 0)
	s.feeModel.Register(settlement.ChainTRON, "USDT", 1, 0)
	// 测试默认桩：kyc_level=2（满足多数规则）；可按用例覆盖 s.kycFetcher。
	s.kycFetcher = func(c *gin.Context) (int, error) { return 2, nil }
	return s
}

// decodeData 解包 {code,data,message} 信封，返回 data 部分（code!=0 时失败）。
func decodeData(t *testing.T, w *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, w.Body.String())
	}
	if env.Code != 0 {
		t.Fatalf("unexpected code %d (body=%s)", env.Code, w.Body.String())
	}
	return env.Data
}

func callFinalize(s *Server, holdID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(gin.H{"hold_id": holdID})
	c.Request, _ = http.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	s.handleWithdrawFinalize(c)
	return w
}

func callApprove(s *Server, holdID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "hold_id", Value: holdID}}
	c.Request, _ = http.NewRequest(http.MethodPost, "/x/"+holdID, nil)
	s.handleWithdrawApprove(c)
	return w
}

func callReject(s *Server, holdID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "hold_id", Value: holdID}}
	c.Request, _ = http.NewRequest(http.MethodPost, "/x/"+holdID, nil)
	s.handleWithdrawReject(c)
	return w
}

// callWithdrawRequest 直接驱动提现受理 handler（含 risk 强制网关）。
func callWithdrawRequest(s *Server, userID int64, asset, chain string, amount, fee float64, address string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(gin.H{
		"user_id": userID, "asset": asset, "chain": chain,
		"amount": amount, "fee": fee, "address": address,
	})
	c.Request, _ = http.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	s.handleWithdrawRequest(c)
	return w
}

// decodeEnv 解析 {code,message} 信封，返回 (code, message)。
func decodeEnv(t *testing.T, w *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, w.Body.String())
	}
	return env.Code, env.Message
}

// TestWithdrawRiskLimitRejected 验证提现超限被 risk 网关拒绝。
func TestWithdrawRiskLimitRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	if _, err := s.riskSvc.AddRule(&risk.RiskRule{
		Kind: risk.KindWithdrawLimit, Asset: "USDT",
		MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")),
		MinKYCLevel:     1,
	}); err != nil {
		t.Fatal(err)
	}
	w := callWithdrawRequest(s, 1, "USDT", "Ethereum", 2000, 0, "0xabc")
	code, msg := decodeEnv(t, w)
	if code != 403 || msg != "exceeds withdraw limit" {
		t.Fatalf("want 403 exceeds withdraw limit, got %d %q", code, msg)
	}
}

// TestWithdrawRiskUserBlacklistRejected 验证用户黑名单被 risk 网关拒绝。
func TestWithdrawRiskUserBlacklistRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	if _, err := s.riskSvc.AddBlacklist("1", risk.BlacklistUser, "fraud"); err != nil {
		t.Fatal(err)
	}
	w := callWithdrawRequest(s, 1, "USDT", "Ethereum", 100, 0, "0xabc")
	code, msg := decodeEnv(t, w)
	if code != 403 || msg != "user blacklisted" {
		t.Fatalf("want 403 user blacklisted, got %d %q", code, msg)
	}
}

// TestWithdrawRiskAddressBlacklistRejected 验证地址黑名单被 risk 网关拒绝。
func TestWithdrawRiskAddressBlacklistRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	if _, err := s.riskSvc.AddBlacklist("0xbad", risk.BlacklistAddress, "sanctioned"); err != nil {
		t.Fatal(err)
	}
	w := callWithdrawRequest(s, 1, "USDT", "Ethereum", 100, 0, "0xbad")
	code, msg := decodeEnv(t, w)
	if code != 403 || msg != "address blacklisted" {
		t.Fatalf("want 403 address blacklisted, got %d %q", code, msg)
	}
}

// TestWithdrawRiskLowKYCRejected 验证 KYC 等级不足被 risk 网关拒绝。
func TestWithdrawRiskLowKYCRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	s.kycFetcher = func(c *gin.Context) (int, error) { return 1, nil } // 覆盖为低 KYC
	if _, err := s.riskSvc.AddRule(&risk.RiskRule{
		Kind: risk.KindWithdrawLimit, Asset: "USDT",
		MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")),
		MinKYCLevel:     2,
	}); err != nil {
		t.Fatal(err)
	}
	w := callWithdrawRequest(s, 1, "USDT", "Ethereum", 100, 0, "0xabc")
	code, msg := decodeEnv(t, w)
	if code != 403 || msg != "kyc level too low" {
		t.Fatalf("want 403 kyc level too low, got %d %q", code, msg)
	}
}

// TestWithdrawNegativeRejected 验证负金额在提现强制路径被阻断（handler 输入校验 + risk 双重保险）。
func TestWithdrawNegativeRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	w := callWithdrawRequest(s, 1, "USDT", "Ethereum", -100, 0, "0xabc")
	if w.Code != 400 {
		t.Fatalf("want 400 for negative amount, got %d %s", w.Code, w.Body.String())
	}
}

// TestWithdrawRiskPass 验证满足规则时正常受理（返回 hold_id）。
func TestWithdrawRiskPass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	if _, err := s.riskSvc.AddRule(&risk.RiskRule{
		Kind: risk.KindWithdrawLimit, Asset: "USDT",
		MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")),
		MinKYCLevel:     1,
	}); err != nil {
		t.Fatal(err)
	}
	w := callWithdrawRequest(s, 1, "USDT", "Ethereum", 100, 0, "0xabc")
	if w.Code != 200 {
		t.Fatalf("want 200, got %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		HoldID string `json:"hold_id"`
	}
	if err := json.Unmarshal(decodeData(t, w), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.HoldID == "" {
		t.Fatalf("expected hold_id, got %+v", resp)
	}
}

// TestWithdrawApproveSkipsCooling 验证管理员 approve 跳过冷静期直接放行，
// 而用户端 finalize 在冷却期内被拒（§25 资金安全缺口闭合）。
func TestWithdrawApproveSkipsCooling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	holdID, _, err := s.ledgerSvc.RequestWithdrawHold(1, "USDT", amt("USDT", 100), amt("USDT", 1), "Ethereum", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	// 冷却期内，用户端 finalize 应被拒（409）。
	if w := callFinalize(s, holdID); w.Code != http.StatusConflict {
		t.Fatalf("finalize during cooling should be 409, got %d", w.Code)
	}
	// 管理员 approve 跳过冷却期，应成功放行（链上广播 + 账本划出）。
	w := callApprove(s, holdID)
	if w.Code != http.StatusOK {
		t.Fatalf("approve should succeed, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		TxHash string `json:"tx_hash"`
	}
	if err := json.Unmarshal(decodeData(t, w), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "approved" || resp.TxHash == "" {
		t.Fatalf("approve response wrong: %+v", resp)
	}
	e, ok := s.ledgerSvc.WithdrawHold(holdID)
	if !ok || !e.Finalized {
		t.Fatalf("hold should be finalized after approve")
	}
}

// TestWithdrawReject 验证管理员 reject 退回冻结资金（不链上广播）。
func TestWithdrawReject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	holdID, _, err := s.ledgerSvc.RequestWithdrawHold(1, "USDT", amt("USDT", 100), amt("USDT", 1), "Ethereum", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	w := callReject(s, holdID)
	if w.Code != http.StatusOK {
		t.Fatalf("reject should succeed, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(decodeData(t, w), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "rejected" {
		t.Fatalf("reject status wrong: %+v", resp)
	}
	e, ok := s.ledgerSvc.WithdrawHold(holdID)
	if !ok || !e.Cancelled {
		t.Fatalf("hold should be cancelled after reject")
	}
}

// TestWithdrawApproveUnknown 验证审批不存在的 hold 返回 404。
func TestWithdrawApproveUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	if w := callApprove(s, "nope"); w.Code != http.StatusNotFound {
		t.Fatalf("approve unknown hold should be 404, got %d", w.Code)
	}
}
