package futuresapi

import (
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// TestLiquidationNoticePublished 验证 §37 强平事件写入用户站内通知。
func TestLiquidationNoticePublished(t *testing.T) {
	s := newGapServer(t)
	s.notifSvc = notification.New(notification.NewMemStore())

	ev := futures.LiquidationEvent{
		UserID:        1,
		Symbol:        "BTC_USDT_PERP",
		Side:          futures.Long,
		Size:          0.5,
		LiqPrice:      48000,
		Fee:           12.5,
		Realized:      -250,
		Partial:       false,
		RemainingSize: 0,
		Time:          time.Now().Unix(),
	}
	s.publishLiquidationNotice(ev)

	ns, err := s.notifSvc.List(1, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(ns))
	}
	if ns[0].Type != notification.TypeLiquidation {
		t.Fatalf("expected liquidation type, got %s", ns[0].Type)
	}
	if ns[0].Title != "合约仓位被强平" {
		t.Fatalf("unexpected title: %s", ns[0].Title)
	}

	// 部分强平应走不同标题。
	s2 := newGapServer(t)
	s2.notifSvc = notification.New(notification.NewMemStore())
	ev2 := ev
	ev2.Partial = true
	ev2.RemainingSize = 0.3
	s2.publishLiquidationNotice(ev2)
	ns2, _ := s2.notifSvc.List(1, false, 10)
	if ns2[0].Title != "合约仓位部分强平" {
		t.Fatalf("partial title mismatch: %s", ns2[0].Title)
	}
}

// TestMarginWarningEmitted 验证 §37 标记价临近强平价时发出保证金预警通知。
func TestMarginWarningEmitted(t *testing.T) {
	s := newGapServer(t)
	s.notifSvc = notification.New(notification.NewMemStore())
	s.marginWarned = make(map[string]bool)
	s.liquidator = futures.NewLiquidator(func(ev futures.LiquidationEvent) {})
	s.liquidator.Register("BTC_USDT_PERP")

	// 逐仓多头：入场 50000，保证金 5000，杠杆 10，持仓 1 张。
	// 标记价 48000 时 UPNL = (48000-50000)*1 = -2000；
	// 保证金率 = (5000 + (-2000)) / (1*48000) = 3000/48000 = 6.25% → 低于阈值 120%，应预警。
	book, ok := s.liquidator.Book("BTC_USDT_PERP")
	if !ok || book == nil {
		t.Fatal("book nil")
	}
	book.Open(1, "BTC_USDT_PERP", futures.Long, 1, 50000, 5000, 10, 0)

	mark := 48000.0
	s.emitMarginWarnings("BTC_USDT_PERP", mark)

	ns, _ := s.notifSvc.List(1, false, 10)
	if len(ns) != 1 {
		t.Fatalf("expected 1 margin warning, got %d", len(ns))
	}
	if ns[0].Type != notification.TypeMarginWarning {
		t.Fatalf("expected margin_warning type, got %s", ns[0].Type)
	}

	// 再次调用应被内存去重，不再新增。
	s.emitMarginWarnings("BTC_USDT_PERP", mark)
	ns2, _ := s.notifSvc.List(1, false, 10)
	if len(ns2) != 1 {
		t.Fatalf("expected dedup (still 1), got %d", len(ns2))
	}

	// 清空去重状态后再次触发可恢复（模拟行情回升后再次下跌）。
	s.marginWarnedMu.Lock()
	delete(s.marginWarned, "1:BTC_USDT_PERP")
	s.marginWarnedMu.Unlock()
	s.emitMarginWarnings("BTC_USDT_PERP", mark)
	ns3, _ := s.notifSvc.List(1, false, 10)
	if len(ns3) != 2 {
		t.Fatalf("expected 2 after dedupe reset, got %d", len(ns3))
	}
}

// TestDepositNoticePublished 验证 §37 链上充值到账写入用户站内通知。
func TestDepositNoticePublished(t *testing.T) {
	s := newGapServer(t)
	s.notifSvc = notification.New(notification.NewMemStore())

	ev := settlement.DepositEvent{
		UserID:  7,
		Chain:   "ETH",
		Asset:   "ETH",
		Amount:  settlement.AssetAmountFromFloat(1.0, 18),
		TxHash:  "0xdepositabc",
		Address: "0xaddr",
	}
	s.publishDepositNotice(ev)

	ns, err := s.notifSvc.List(7, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(ns))
	}
	if ns[0].Type != notification.TypeDepositArrived {
		t.Fatalf("expected deposit_arrived type, got %s", ns[0].Type)
	}
	if ns[0].Title != "充值到账" {
		t.Fatalf("unexpected title: %s", ns[0].Title)
	}
}

// TestWithdrawNoticePublished 验证 §37 链上提现完成写入用户站内通知。
func TestWithdrawNoticePublished(t *testing.T) {
	s := newGapServer(t)
	s.notifSvc = notification.New(notification.NewMemStore())

	ev := settlement.WithdrawEvent{
		UserID:  8,
		Chain:   "ETH",
		Asset:   "USDT",
		Amount:  settlement.AssetAmountFromFloat(100.0, 6),
		Fee:     settlement.AssetAmountFromFloat(1.0, 6),
		TxHash:  "0xwithdrawxyz",
		Status:  settlement.WithdrawCredited,
	}
	s.publishWithdrawNotice(ev)

	ns, err := s.notifSvc.List(8, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(ns))
	}
	if ns[0].Type != notification.TypeWithdrawDone {
		t.Fatalf("expected withdraw_done type, got %s", ns[0].Type)
	}
	if ns[0].Title != "提现已完成" {
		t.Fatalf("unexpected title: %s", ns[0].Title)
	}
}
