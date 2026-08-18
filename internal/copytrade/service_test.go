package copytrade

import (
	"context"
	"testing"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// mockExec 记录代粉丝下单调用，并校验 F1(client_oid)/F4(token) 绑定。
type mockExec struct {
	calls []execCall
}

type execCall struct {
	token    string
	market   string
	symbol   string
	side     string
	price    float64
	qty      float64
	clientOID string
}

func (m *mockExec) Execute(_ context.Context, token, market, symbol, side string, price, qty float64, clientOID string) (string, error) {
	m.calls = append(m.calls, execCall{token, market, symbol, side, price, qty, clientOID})
	return "ex-ord-copy", nil
}

func newTestService() (*Service, *mockExec, *MemStore, *ledger.Ledger) {
	store := NewMemStore()
	mock := &mockExec{}
	lg := ledger.New()
	// 粉丝(follower=2)在 copytrade 账本持有 USDT，供平台复制费结算。
	_ = lg.ReceiveOnChain(2, "USDT", settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")), "seed")
	svc := NewService(store, lg, mock, Config{MinNotional: 1, CopyFeeRate: 0.001}, nil)
	return svc, mock, store, lg
}

// F4：非本人不得关闭带单/停止跟单。
func TestCloseStopF4Owner(t *testing.T) {
	svc, _, _, _ := newTestService()
	if _, err := svc.CreateLead(1, "alice", ""); err != nil {
		t.Fatalf("create lead: %v", err)
	}
	if err := svc.CloseLead(2, 1); err != ErrNotOwner {
		t.Errorf("CloseLead by other: want ErrNotOwner, got %v", err)
	}
	f, err := svc.RegisterFollow(2, 1, 1, 0, "tok-follower-2")
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if err := svc.StopFollow(3, f.ID); err != ErrNotOwner {
		t.Errorf("StopFollow by other: want ErrNotOwner, got %v", err)
	}
}

// F1/F4/F5 + 平台复制费：lead 成交触发粉丝复制下单 + 复制费结算入 SysCopyTradeFee。
func TestOnTradeReplicatesAndFees(t *testing.T) {
	svc, mock, _, lg := newTestService()
	if _, err := svc.CreateLead(1, "alice", ""); err != nil {
		t.Fatalf("create lead: %v", err)
	}
	if _, err := svc.RegisterFollow(2, 1, 0.5, 0, "tok-follower-2"); err != nil {
		t.Fatalf("follow: %v", err)
	}

	ev := mq.TradeEvent{Symbol: "BTC_USDT", Price: 10000, Qty: 2, TakerID: 1, MakerID: 99, TakerSide: "buy", Ts: 1700000000000}
	svc.OnTrade(context.Background(), ev)

	if len(mock.calls) != 1 {
		t.Fatalf("want 1 follower order, got %d", len(mock.calls))
	}
	call := mock.calls[0]
	// F4：代下单须携带粉丝授权 token。
	if call.token != "tok-follower-2" {
		t.Errorf("F4: order should carry follower token, got %q", call.token)
	}
	// 复制方向应与 lead（taker）一致 = buy。
	if call.side != "buy" {
		t.Errorf("side: want buy, got %s", call.side)
	}
	// 复制名义额 = lead 名义额(20000) * ratio(0.5) = 10000；qty = 10000/10000 = 1.0 BTC。
	if call.qty < 0.99 || call.qty > 1.01 {
		t.Errorf("qty: want ~1.0, got %v", call.qty)
	}
	// F1：client_oid 形如 copytrade:<followID>:<eventID>，下游据此外部去重。
	if len(call.clientOID) == 0 || call.clientOID[:10] != "copytrade:" {
		t.Errorf("F1: client_oid malformed: %q", call.clientOID)
	}

	// 平台复制费 = 10000 * 0.001 = 10 USDT 应结算入 SysCopyTradeFee。
	feeAvail, _, ok := lg.Balance(ledger.SysCopyTradeFee, "USDT")
	if !ok || feeAvail.Value == nil {
		t.Fatalf("SysCopyTradeFee not credited (transfer silently failed); ok=%v", ok)
	}
	if feeAvail.Value.Int64() != 10 * int64(1e6) { // USDT decimals=6 -> 10 USDT = 10_000_000
		t.Errorf("SysCopyTradeFee balance: want 10 USDT, got %v", feeAvail)
	}

	// F1 幂等：同一事件再次到达应被全局去重，不再下单/不再收费。
	svc.OnTrade(context.Background(), ev)
	if len(mock.calls) != 1 {
		t.Errorf("F1: duplicate event should not re-replicate, calls=%d", len(mock.calls))
	}
}

// F5：计价资产未知的交易对（如 PERP）跳过复制，不收费、不下单。
func TestOnTradeSkipsUnsupportedQuote(t *testing.T) {
	svc, mock, _, lg := newTestService()
	if _, err := svc.CreateLead(1, "alice", ""); err != nil {
		t.Fatalf("create lead: %v", err)
	}
	if _, err := svc.RegisterFollow(2, 1, 1, 0, "tok-follower-2"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	ev := mq.TradeEvent{Symbol: "BTC_USDT_PERP", Price: 10000, Qty: 1, TakerID: 1, MakerID: 99, TakerSide: "buy", Ts: 1}
	svc.OnTrade(context.Background(), ev)
	if len(mock.calls) != 0 {
		t.Errorf("F5: unsupported quote should skip, calls=%d", len(mock.calls))
	}
	_, feeBal, _ := lg.Balance(ledger.SysCopyTradeFee, "USDT")
	if !feeBal.IsZero() {
		t.Errorf("F5: no fee should be charged for skipped trade, fee=%v", feeBal)
	}
}

// F5：名义额低于下限的粉尘单不复制。
func TestOnTradeSkipsBelowMin(t *testing.T) {
	svc, mock, _, _ := newTestService()
	svc.cfg.MinNotional = 1000 // 设高下限
	if _, err := svc.CreateLead(1, "alice", ""); err != nil {
		t.Fatalf("create lead: %v", err)
	}
	if _, err := svc.RegisterFollow(2, 1, 0.01, 0, "tok-follower-2"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	ev := mq.TradeEvent{Symbol: "BTC_USDT", Price: 100, Qty: 1, TakerID: 1, MakerID: 99, TakerSide: "buy", Ts: 1}
	svc.OnTrade(context.Background(), ev) // 名义额=100*1*0.01=1 < 1000
	if len(mock.calls) != 0 {
		t.Errorf("F5: below-min notional should skip, calls=%d", len(mock.calls))
	}
}

// F4：被跟单者为 maker 时，复制方向应取其对手方向。
func TestOnTradeMakerSideReversal(t *testing.T) {
	svc, mock, _, _ := newTestService()
	if _, err := svc.CreateLead(1, "alice", ""); err != nil {
		t.Fatalf("create lead: %v", err)
	}
	if _, err := svc.RegisterFollow(2, 1, 1, 0, "tok-follower-2"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	// lead=1 是 maker，taker 是别人；maker 成交方向与 taker 相反。
	ev := mq.TradeEvent{Symbol: "BTC_USDT", Price: 100, Qty: 1, TakerID: 7, MakerID: 1, TakerSide: "buy", Ts: 1}
	svc.OnTrade(context.Background(), ev)
	if len(mock.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(mock.calls))
	}
	if mock.calls[0].side != "sell" {
		t.Errorf("maker lead should be reversed to sell, got %s", mock.calls[0].side)
	}
}
