package mq

import (
	"context"
	"testing"
	"time"
)

func TestInMemPublisherBuffersAndHandles(t *testing.T) {
	var got []TradeEvent
	p := NewInMemPublisher(8, func(_ context.Context, ev TradeEvent) {
		got = append(got, ev)
	})
	want := TradeEvent{Symbol: "BTCUSDT", Price: 50000, Qty: 1, TakerID: 1, MakerID: 2, TakerSide: "buy", Ts: time.Now().UnixMilli()}
	if err := p.PublishTrade(context.Background(), want); err != nil {
		t.Fatalf("publish: %v", err)
	}
	drained := p.Drain()
	if len(drained) != 1 || drained[0].Symbol != "BTCUSDT" {
		t.Fatalf("drain mismatch: %+v", drained)
	}
	if len(got) != 1 || got[0].MakerID != 2 {
		t.Fatalf("handler not invoked correctly: %+v", got)
	}
}

func TestInMemPublisherDropsOldestWhenFull(t *testing.T) {
	p := NewInMemPublisher(2, nil)
	_ = p.PublishTrade(context.Background(), TradeEvent{Symbol: "a"})
	_ = p.PublishTrade(context.Background(), TradeEvent{Symbol: "b"})
	_ = p.PublishTrade(context.Background(), TradeEvent{Symbol: "c"}) // 应挤掉 a
	d := p.Drain()
	if len(d) != 2 || d[0].Symbol != "b" || d[1].Symbol != "c" {
		t.Fatalf("expected [b,c], got %+v", d)
	}
}

func TestInMemPublisherDepthBuffers(t *testing.T) {
	p := NewInMemPublisher(4, nil)
	want := DepthEvent{Symbol: "BTCUSDT", Bids: []DepthLevel{{Price: 50000, Volume: 1}}, Ts: 1}
	if err := p.PublishDepth(context.Background(), want); err != nil {
		t.Fatalf("publish depth: %v", err)
	}
	drained := p.DrainDepth()
	if len(drained) != 1 || drained[0].Symbol != "BTCUSDT" || len(drained[0].Bids) != 1 {
		t.Fatalf("drain depth mismatch: %+v", drained)
	}
	// 成交缓冲不应被深度缓冲污染。
	if len(p.Drain()) != 0 {
		t.Fatal("trade buffer should be empty after only depth publishes")
	}
}

func TestInMemSubscriberRoundTrip(t *testing.T) {
	var gotTopic string
	var gotData []byte
	sub := NewInMemSubscriber(func(_ context.Context, topic string, data []byte) error {
		gotTopic = topic
		gotData = append([]byte(nil), data...)
		return nil
	})
	if err := sub.Feed("exchange.trades", []byte(`{"symbol":"BTCUSDT"}`)); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if gotTopic != "exchange.trades" {
		t.Fatalf("expected topic exchange.trades, got %q", gotTopic)
	}
	if string(gotData) != `{"symbol":"BTCUSDT"}` {
		t.Fatalf("unexpected payload: %s", gotData)
	}
}

func TestTradeEventJSON(t *testing.T) {
	ev := TradeEvent{Symbol: "ETHUSDT", Price: 3000, Qty: 2}
	b, err := ev.JSON()
	if err != nil || len(b) == 0 {
		t.Fatalf("json marshal: %v len=%d", err, len(b))
	}
}
