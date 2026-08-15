package spot

import (
	"fmt"
	"testing"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"go.uber.org/zap"
)

// newTestServer 构造一个不依赖实时撮合服务的现货 Server，仅用于结算单元测试。
func newTestServer() *Server {
	return &Server{
		log:        zap.NewNop(),
		ledgerSvc:  ledger.New(),
		openOrders: make(map[int64]*freezeRec),
	}
}

// seed 通过链上充值（复式记账：Debit 负债账户 + Credit 用户）注入余额，保证账本全局平衡，
// 使 IsBalanced 断言有效。
func seed(s *Server, uid int64, usdt, btc float64) {
	_ = s.ledgerSvc.ReceiveOnChain(uid, "USDT", usdt, fmt.Sprintf("seed:%d:USDT", uid))
	_ = s.ledgerSvc.ReceiveOnChain(uid, "BTC", btc, fmt.Sprintf("seed:%d:BTC", uid))
}

// 测试1：限价买单在撮合前预冻结计价资产，账本保持平衡。
func TestReserveOnOpenBuyLimit(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0)

	rec, err := s.reserveOnOpen(1, matching.Buy, 100, 1, "BTC_USDT")
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if rec.frozenQuote != 100 {
		t.Fatalf("expect frozenQuote=100, got %v", rec.frozenQuote)
	}
	avail, frozen, _ := s.ledgerSvc.Balance(1, "USDT")
	if avail != 99900 || frozen != 100 {
		t.Fatalf("balance wrong: avail=%v frozen=%v", avail, frozen)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatalf("ledger unbalanced after reserve")
	}
}

// 测试2：余额不足时预冻结失败，且不应冻结任何资金。
func TestReserveOnOpenInsufficient(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 50, 0) // 仅 50 USDT

	if _, err := s.reserveOnOpen(1, matching.Buy, 100, 1, "BTC_USDT"); err == nil {
		t.Fatalf("expect insufficient error")
	}
	avail, frozen, _ := s.ledgerSvc.Balance(1, "USDT")
	if avail != 50 || frozen != 0 {
		t.Fatalf("balance should be untouched: avail=%v frozen=%v", avail, frozen)
	}
}

// 测试3：一笔限价买/卖成交后，买卖双方余额正确变动，冻结清零，账本平衡。
func TestSettleFillLimitBuySell(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0) // 买方：只有 USDT
	seed(s, 2, 0, 10)     // 卖方：只有 BTC

	// 预冻结：买方冻结 100 USDT，卖方冻结 1 BTC。
	buyRec, _ := s.reserveOnOpen(1, matching.Buy, 100, 1, "BTC_USDT")
	sellRec, _ := s.reserveOnOpen(2, matching.Sell, 100, 1, "BTC_USDT")
	s.openOrders[101] = buyRec
	s.openOrders[202] = sellRec

	trade := matching.Trade{
		Price:     100,
		Qty:       1,
		TakerSide: matching.Buy,
		TakerID:   1,
		MakerID:   2,
		TakerOID:  101,
		MakerOID:  202,
	}
	if err := s.settleFill("BTC_USDT", trade); err != nil {
		t.Fatalf("settleFill failed: %v", err)
	}

	// 买方：USDT 100→0（冻结→解冻→划转给卖方），BTC 0→1。
	bA, bF, _ := s.ledgerSvc.Balance(1, "USDT")
	if bA != 99900 || bF != 0 {
		t.Fatalf("buyer USDT wrong: avail=%v frozen=%v", bA, bF)
	}
	if b, _, _ := s.ledgerSvc.Balance(1, "BTC"); b != 1 {
		t.Fatalf("buyer BTC should be 1, got %v", b)
	}
	// 卖方：BTC 10→9，USDT 0→100。
	sA, sF, _ := s.ledgerSvc.Balance(2, "BTC")
	if sA != 9 || sF != 0 {
		t.Fatalf("seller BTC wrong: avail=%v frozen=%v", sA, sF)
	}
	if u, _, _ := s.ledgerSvc.Balance(2, "USDT"); u != 100 {
		t.Fatalf("seller USDT should be 100, got %v", u)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatalf("ledger unbalanced after settle")
	}
	// 完全成交，记录应被清理。
	s.freezeMu.Lock()
	n := len(s.openOrders)
	s.freezeMu.Unlock()
	if n != 0 {
		t.Fatalf("openOrders should be empty after full fill, got %d", n)
	}
}

// 测试4：撤单释放剩余预冻结，恢复可用余额。
func TestCancelReleasesFrozen(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0)

	rec, _ := s.reserveOnOpen(1, matching.Buy, 100, 2, "BTC_USDT") // 冻结 200 USDT
	s.openOrders[777] = rec
	if _, f, _ := s.ledgerSvc.Balance(1, "USDT"); f != 200 {
		t.Fatalf("expect frozen=200 before cancel, got %v", f)
	}

	// 模拟撤单释放（与 handleCancel 逻辑一致）。
	s.freezeMu.Lock()
	r, ok := s.openOrders[777]
	if ok {
		s.releaseRemaining(r)
		delete(s.openOrders, 777)
	}
	s.freezeMu.Unlock()

	avail, frozen, _ := s.ledgerSvc.Balance(1, "USDT")
	if avail != 100000 || frozen != 0 {
		t.Fatalf("after cancel expect avail=100000 frozen=0, got avail=%v frozen=%v", avail, frozen)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatalf("ledger unbalanced after cancel")
	}
}

// 测试5：部分成交后，剩余预冻结随撤单释放，且已成交部分已结算。
func TestPartialFillThenCancel(t *testing.T) {
	s := newTestServer()
	seed(s, 1, 100000, 0)

	rec, _ := s.reserveOnOpen(1, matching.Buy, 100, 2, "BTC_USDT") // 冻结 200 USDT
	s.openOrders[888] = rec

	// 半成交 1 BTC @100：买方付 100 USDT 给卖方（此处卖方任意，重点在看买方账本）。
	trade := matching.Trade{
		Price:     100, Qty: 1, TakerSide: matching.Buy,
		TakerID: 1, MakerID: 99, TakerOID: 888, MakerOID: 999,
	}
	if err := s.settleFill("BTC_USDT", trade); err != nil {
		t.Fatalf("settleFill failed: %v", err)
	}
	// 买方已付 100 USDT，剩余冻结应为 100。
	if _, f, _ := s.ledgerSvc.Balance(1, "USDT"); f != 100 {
		t.Fatalf("expect frozen=100 after partial fill, got %v", f)
	}
	// 撤单释放剩余 100。
	s.freezeMu.Lock()
	r, ok := s.openOrders[888]
	if ok {
		s.releaseRemaining(r)
		delete(s.openOrders, 888)
	}
	s.freezeMu.Unlock()
	if avail, f, _ := s.ledgerSvc.Balance(1, "USDT"); avail != 99900 || f != 0 {
		t.Fatalf("after cancel expect avail=99900 frozen=0, got avail=%v frozen=%v", avail, f)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatalf("ledger unbalanced")
	}
}
