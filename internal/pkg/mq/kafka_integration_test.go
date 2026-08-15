//go:build kafka

package mq

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestKafkaPublishConsume 是 Kafka 生产/消费闭环的集成测试，仅在 -tags kafka 构建且设置了
// KAFKA_TEST_BROKERS 时运行（例如本地 Docker 单节点 Kafka）。未设置则跳过，不污染 CI。
//
// 依赖 broker 自动建 topic（Kafka 默认 auto.create.topics.enable=true）。
func TestKafkaPublishConsume(t *testing.T) {
	raw := os.Getenv("KAFKA_TEST_BROKERS")
	if raw == "" {
		t.Skip("set KAFKA_TEST_BROKERS to run Kafka integration test")
	}
	brokers := strings.Split(raw, ",")
	tradeTopic := "ce_test_trades"
	depthTopic := "ce_test_depth"

	pub, err := NewKafkaPublisher(brokers, tradeTopic, depthTopic)
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	defer pub.Close()

	sub, err := NewKafkaSubscriber(brokers, "ce-test-group")
	if err != nil {
		t.Fatalf("new subscriber: %v", err)
	}
	defer sub.Close()

	got := make(chan TradeEvent, 1)
	handler := func(_ context.Context, topic string, data []byte) error {
		if topic == tradeTopic {
			var ev TradeEvent
			if err := json.Unmarshal(data, &ev); err == nil {
				got <- ev
			}
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Subscribe(ctx, []string{tradeTopic, depthTopic}, handler) }()

	// 消费组以 OffsetNewest 加入：只会消费「建立 offset 之后」发布的消息。
	// 反复发布直到被消费，避免依赖消费组 join 完成的精确时机（不同环境 join 耗时不一）。
	want := TradeEvent{Symbol: "BTCUSDT", Price: 50000, Qty: 1, TakerID: 1, MakerID: 2, TakerSide: "buy", Ts: time.Now().UnixMilli()}
	deadline := time.Now().Add(25 * time.Second)
	for {
		if err := pub.PublishTrade(context.Background(), want); err != nil {
			t.Fatalf("publish trade: %v", err)
		}
		select {
		case ev := <-got:
			if ev.Symbol != want.Symbol || ev.Price != want.Price || ev.MakerID != want.MakerID {
				t.Fatalf("consumed trade mismatch: %+v", ev)
			}
			return
		case <-time.After(1 * time.Second):
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for consumed trade")
			}
		}
	}
}
