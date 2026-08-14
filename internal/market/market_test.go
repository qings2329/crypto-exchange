package market

import (
	"testing"

	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
)

func TestMarketUpdateAndSnapshot(t *testing.T) {
	m := NewMarket()
	if m.Snapshot("BTCUSDT") != nil {
		t.Fatal("expected no ticker before any update")
	}
	m.Update(mq.TradeEvent{Symbol: "BTCUSDT", Price: 50000, Qty: 2, Ts: 1})
	s := m.Snapshot("BTCUSDT")
	if s == nil {
		t.Fatal("expected ticker after update")
	}
	if s.Last != 50000 {
		t.Fatalf("last price = %v", s.Last)
	}
	// 买/卖盘口应在成交价两侧，且 ask > bid。
	if !(s.BestAsk > s.BestBid) {
		t.Fatalf("ask must exceed bid: bid=%v ask=%v", s.BestBid, s.BestAsk)
	}
	if len(m.Symbols()) != 1 || m.Symbols()[0] != "BTCUSDT" {
		t.Fatalf("symbols = %v", m.Symbols())
	}
}

func TestMarketUpdateRefreshesLast(t *testing.T) {
	m := NewMarket()
	m.Update(mq.TradeEvent{Symbol: "ETHUSDT", Price: 3000, Ts: 1})
	m.Update(mq.TradeEvent{Symbol: "ETHUSDT", Price: 3100, Ts: 2})
	if s := m.Snapshot("ETHUSDT"); s.Last != 3100 {
		t.Fatalf("expected last refreshed to 3100, got %v", s.Last)
	}
}
