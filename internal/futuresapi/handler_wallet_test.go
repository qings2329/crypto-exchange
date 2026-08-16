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
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// newWithdrawServer 构造最小 futures Server（仅钱包提现相关字段），用于提币审批测试。
// 账户 1 充值 10000 USDT，冷却期 30s、地址验证冷静期 0、每日限额 50000，覆盖 approve/reject 场景。
func newWithdrawServer(t *testing.T) *Server {
	t.Helper()
	l := ledger.New()
	if err := l.Deposit(1, "USDT", 10000, "seed"); err != nil {
		t.Fatal(err)
	}
	l.SetWithdrawHoldPeriod(30 * time.Second)
	l.SetAddressVerifyPeriod(0) // 测试中确认地址后立即可用
	l.SetDailyWithdrawLimit("USDT", 50000)
	if _, err := l.AddWithdrawAddress(1, "USDT", "Ethereum", "0xabc", "test"); err != nil {
		t.Fatal(err)
	}
	if err := l.ConfirmWithdrawAddress(1, "USDT", "Ethereum", "0xabc"); err != nil {
		t.Fatal(err)
	}
	gw := settlement.NewMockWithdrawGateway(3, time.Second)
	return &Server{ledgerSvc: l, chainWithdraw: gw}
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

// TestWithdrawApproveSkipsCooling 验证管理员 approve 跳过冷静期直接放行，
// 而用户端 finalize 在冷却期内被拒（§25 资金安全缺口闭合）。
func TestWithdrawApproveSkipsCooling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newWithdrawServer(t)
	holdID, _, err := s.ledgerSvc.RequestWithdrawHold(1, "USDT", 100, 1, "Ethereum", "0xabc")
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
	holdID, _, err := s.ledgerSvc.RequestWithdrawHold(1, "USDT", 100, 1, "Ethereum", "0xabc")
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
