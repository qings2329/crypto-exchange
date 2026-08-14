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

func TestNewPublisherFallsBackToInMem(t *testing.T) {
	// 默认（无 kafka tag）即使给了 brokers 也退回内存发布器，保证离线可运行。
	p := NewPublisher([]string{"127.0.0.1:9092"}, "trades", nil)
	if _, ok := p.(*InMemPublisher); !ok {
		t.Fatalf("expected InMemPublisher fallback, got %T", p)
	}
}

func TestTradeEventJSON(t *testing.T) {
	ev := TradeEvent{Symbol: "ETHUSDT", Price: 3000, Qty: 2}
	b, err := ev.JSON()
	if err != nil || len(b) == 0 {
		t.Fatalf("json marshal: %v len=%d", err, len(b))
	}
}
