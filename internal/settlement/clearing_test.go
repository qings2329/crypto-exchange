package settlement

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
)

func sampleTrade() mq.TradeEvent {
	return mq.TradeEvent{
		Symbol: "BTC_USDT", Price: 50000, Qty: 2,
		TakerID: 1, MakerID: 2, TakerSide: "buy", Ts: 1700000000000,
	}
}

// TestClearerRecordsAndAggregates 验证单笔成交清算入账、手续费计算与聚合统计。
func TestClearerRecordsAndAggregates(t *testing.T) {
	store := NewMemClearingStore(0)
	cl := NewClearer(store, 0.001)

	if err := cl.Clear(sampleTrade()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	recs, err := cl.Recent(10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recent: len=%d err=%v", len(recs), err)
	}
	if recs[0].Fee != 50000*2*0.001 {
		t.Fatalf("fee = %v, want %v", recs[0].Fee, 50000*2*0.001)
	}
	st := cl.Stats()
	if st.TotalTrades != 1 || st.TotalVolume != 100000 {
		t.Fatalf("stats = %+v", st)
	}
	if st.TotalCommission != 100 {
		t.Fatalf("commission = %v, want 100", st.TotalCommission)
	}
	if st.BySymbol["BTC_USDT"] != 100 {
		t.Fatalf("by_symbol = %v", st.BySymbol)
	}
}

// TestClearerIdempotent 验证同笔成交重复清算仅计入一次（Kafka at-least-once 去重）。
func TestClearerIdempotent(t *testing.T) {
	store := NewMemClearingStore(0)
	cl := NewClearer(store, 0.001)

	ev := sampleTrade()
	if err := cl.Clear(ev); err != nil {
		t.Fatal(err)
	}
	// 同一成交再次投递（字段完全相同 -> 幂等键相同）。
	if err := cl.Clear(ev); err != nil {
		t.Fatal(err)
	}
	recs, _ := cl.Recent(10)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record after dup, got %d", len(recs))
	}
	if st := cl.Stats(); st.TotalTrades != 1 {
		t.Fatalf("expected 1 trade after dup, got %d", st.TotalTrades)
	}
}

// TestClearerViaSubscriber 验证经由 InMemSubscriber 的 Kafka 消费路径（解包 JSON）正确入账。
func TestClearerViaSubscriber(t *testing.T) {
	store := NewMemClearingStore(0)
	cl := NewClearer(store, 0.001)

	sub := mq.NewInMemSubscriber(func(ctx context.Context, topic string, data []byte) error {
		if topic != "exchange.trades" {
			return nil
		}
		var ev mq.TradeEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return err
		}
		return cl.Clear(ev)
	})

	payload, _ := json.Marshal(sampleTrade())
	if err := sub.Feed("exchange.trades", payload); err != nil {
		t.Fatalf("feed: %v", err)
	}
	recs, _ := cl.Recent(10)
	if len(recs) != 1 {
		t.Fatalf("expected 1 cleared trade via subscriber, got %d", len(recs))
	}
	// 重复投递应被幂等跳过。
	_ = sub.Feed("exchange.trades", payload)
	if recs, _ := cl.Recent(10); len(recs) != 1 {
		t.Fatalf("expected idempotent skip, got %d", len(recs))
	}
}

// TestTradeIDStable 验证相同成交字段产生稳定幂等键，不同字段产生不同键。
func TestTradeIDStable(t *testing.T) {
	a := TradeID(sampleTrade())
	b := TradeID(sampleTrade())
	if a != b {
		t.Fatalf("same trade should yield same id: %d vs %d", a, b)
	}
	ev2 := sampleTrade()
	ev2.Qty = 3
	if TradeID(ev2) == a {
		t.Fatal("different qty should yield different id")
	}
}
