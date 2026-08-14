// Package mq 提供成交流的消息发布抽象，解耦撮合引擎与清算/记账服务。
//
// 设计：撮合引擎每产生一笔成交，调用 Publisher.PublishTrade 发布 TradeEvent。
// 生产环境由 KafkaPublisher 投递到 Kafka（清算服务消费后记账）；
// 无 Kafka 或本地开发时退回 InMemPublisher（缓冲 + 可选回调，便于端到端测试）。
//
// 表名约定：本项目所有库表以 ce_ 开头（见 docs/CONVENTIONS.md）。
package mq

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// TradeEvent 是成交流事件，供清算/记账/行情快照消费。
type TradeEvent struct {
	Symbol    string  `json:"symbol"`
	Price     float64 `json:"price"`
	Qty       float64 `json:"qty"`
	TakerID   int64   `json:"taker_id"`
	MakerID   int64   `json:"maker_id"`
	TakerSide string  `json:"taker_side"` // buy/sell
	Ts        int64   `json:"ts"`         // unix 毫秒
}

// Publisher 成交流发布接口。
type Publisher interface {
	// PublishTrade 发布一笔成交；返回 error 表示发布失败（调用方应记录但不阻断撮合）。
	PublishTrade(ctx context.Context, ev TradeEvent) error
	Close() error
}

// Handler 是成交的本地处理钩子（清算/记账等服务实现），供 InMemPublisher 回调。
type Handler func(ctx context.Context, ev TradeEvent)

// kafkaConstructor 在启用 kafka build tag 时被赋值（见 kafka.go）；否则为 nil，
// NewPublisher 退回 InMemPublisher。这样默认构建不含 sarama 依赖，离线可编译可测。
var kafkaConstructor func(brokers []string, topic string) (Publisher, error)

// NewPublisher 按配置选择发布器：配置了 brokers 且以 -tags kafka 构建时使用 Kafka，
// 否则退回内存发布器（并保留 handler 回调）。失败自动降级，永不阻断撮合。
func NewPublisher(brokers []string, tradeTopic string, handler Handler) Publisher {
	if len(brokers) > 0 && kafkaConstructor != nil {
		if p, err := kafkaConstructor(brokers, tradeTopic); err == nil {
			return p
		}
		// Kafka 初始化失败 → 降级内存，保证服务可用。
	}
	return NewInMemPublisher(1024, handler)
}

// InMemPublisher 内存发布器：缓冲事件并同步调用可选 handler。
// 用于本地开发、单测，以及 Kafka 不可用时的降级路径。
type InMemPublisher struct {
	mu      sync.Mutex
	buf     []TradeEvent
	handler Handler
	cap     int
}

// NewInMemPublisher 构造内存发布器；handler 为 nil 时仅缓冲不回调。
func NewInMemPublisher(capacity int, handler Handler) *InMemPublisher {
	if capacity <= 0 {
		capacity = 1024
	}
	return &InMemPublisher{cap: capacity, handler: handler}
}

// PublishTrade 缓冲事件并执行 handler（同步、best-effort）。
func (p *InMemPublisher) PublishTrade(ctx context.Context, ev TradeEvent) error {
	p.mu.Lock()
	if len(p.buf) >= p.cap {
		// 丢弃最旧，保证不阻塞撮合。
		p.buf = p.buf[1:]
	}
	p.buf = append(p.buf, ev)
	h := p.handler
	p.mu.Unlock()
	if h != nil {
		h(ctx, ev)
	}
	return nil
}

// Drain 取出并清空当前缓冲（测试/对账用）。
func (p *InMemPublisher) Drain() []TradeEvent {
	p.mu.Lock()
	out := append([]TradeEvent(nil), p.buf...)
	p.buf = nil
	p.mu.Unlock()
	return out
}

// Close 内存发布器无需释放资源。
func (p *InMemPublisher) Close() error { return nil }

// JSON 序列化辅助（Kafka 投递与日志共用）。
func (ev TradeEvent) JSON() ([]byte, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("marshal trade event: %w", err)
	}
	return b, nil
}
