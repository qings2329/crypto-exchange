//go:build !kafka

package mq

import "testing"

// 以下两个用例只在「默认构建（无 -tags kafka）」下运行：
// 默认构建未注册 kafkaConstructor / kafkaSubscriberConstructor，NewPublisher / NewSubscriber
// 即使拿到 brokers 也退回内存实现，保证离线可编译可测。启用 kafka tag 时这两个构造函数
// 会被赋值（见 kafka.go），按设计优先返回 Kafka 实现，故不再走 fallback。

func TestNewPublisherFallsBackToInMem(t *testing.T) {
	p := NewPublisher([]string{"127.0.0.1:9092"}, "trades", "depth", nil)
	if _, ok := p.(*InMemPublisher); !ok {
		t.Fatalf("expected InMemPublisher fallback, got %T", p)
	}
}

func TestNewSubscriberFallsBackToInMem(t *testing.T) {
	sub := NewSubscriber([]string{"127.0.0.1:9092"}, "g", nil)
	if _, ok := sub.(*InMemSubscriber); !ok {
		t.Fatalf("expected InMemSubscriber fallback, got %T", sub)
	}
}
