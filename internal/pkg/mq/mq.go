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

// DepthLevel 是深度聚合后的一档（价格 + 该档总挂单量），用于序列化与前端渲染。
// 与撮合引擎内部的 matching.Level（含原始订单队列）不同，这里是已聚合的视图。
type DepthLevel struct {
	Price  float64 `json:"price"`
	Volume float64 `json:"volume"`
}

// DepthEvent 是订单簿深度快照（节流后按交易对发布），供行情服务构建盘口。
type DepthEvent struct {
	Symbol string       `json:"symbol"`
	Bids   []DepthLevel `json:"bids"`
	Asks   []DepthLevel `json:"asks"`
	Ts     int64        `json:"ts"` // unix 毫秒
}

// Publisher 成交流/深度流发布接口。
type Publisher interface {
	// PublishTrade 发布一笔成交；返回 error 表示发布失败（调用方应记录但不阻断撮合）。
	PublishTrade(ctx context.Context, ev TradeEvent) error
	// PublishDepth 发布某交易对的深度快照（节流后调用）；失败仅记录，不阻断撮合。
	PublishDepth(ctx context.Context, ev DepthEvent) error
	Close() error
}

// Handler 是成交的本地处理钩子（清算/记账等服务实现），供 InMemPublisher 回调。
type Handler func(ctx context.Context, ev TradeEvent)

// kafkaConstructor 在启用 kafka build tag 时被赋值（见 kafka.go）；否则为 nil，
// NewPublisher 退回 InMemPublisher。这样默认构建不含 sarama 依赖，离线可编译可测。
var kafkaConstructor func(brokers []string, tradeTopic, depthTopic string) (Publisher, error)

// MsgHandler 消费侧回调：收到一条消息（topic 标识来源，如 exchange.trades / market.depth）。
type MsgHandler func(ctx context.Context, topic string, data []byte) error

// Subscriber 消息订阅接口（成交流/深度流的消费方）。
type Subscriber interface {
	// Subscribe 订阅给定 topic 列表；对每个消息调用 handler（按 envelope 分发 trade/depth）。
	// 阻塞直到 ctx 取消或出错。
	Subscribe(ctx context.Context, topics []string, handler MsgHandler) error
	Close() error
}

// kafkaSubscriberConstructor 在启用 kafka build tag 时被赋值（见 kafka.go）；否则为 nil，
// NewSubscriber 退回 InMemSubscriber。这样默认构建不含 sarama 依赖，离线可编译可测。
var kafkaSubscriberConstructor func(brokers []string, group string) (Subscriber, error)

// NewSubscriber 按配置选择订阅器：配置了 brokers 且以 -tags kafka 构建时使用 Kafka 消费组，
// 否则退回内存订阅器（仅单测/回退用，生产回退路径由调用方改用 WebSocket 行情流）。失败自动降级。
func NewSubscriber(brokers []string, group string, handler MsgHandler) Subscriber {
	if len(brokers) > 0 && kafkaSubscriberConstructor != nil {
		if s, err := kafkaSubscriberConstructor(brokers, group); err == nil {
			return s
		}
		// Kafka 初始化失败 → 降级内存，保证服务可用。
	}
	return NewInMemSubscriber(handler)
}

// NewPublisher 按配置选择发布器：配置了 brokers 且以 -tags kafka 构建时使用 Kafka，
// 否则退回内存发布器（并保留 handler 回调）。失败自动降级，永不阻断撮合。
func NewPublisher(brokers []string, tradeTopic, depthTopic string, handler Handler) Publisher {
	if len(brokers) > 0 && kafkaConstructor != nil {
		if p, err := kafkaConstructor(brokers, tradeTopic, depthTopic); err == nil {
			return p
		}
		// Kafka 初始化失败 → 降级内存，保证服务可用。
	}
	return NewInMemPublisher(1024, handler)
}

// InMemPublisher 内存发布器：缓冲事件并同步调用可选 handler。
// 用于本地开发、单测，以及 Kafka 不可用时的降级路径。
type InMemPublisher struct {
	mu       sync.Mutex
	buf      []TradeEvent
	depthBuf []DepthEvent
	handler  Handler
	cap      int
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

// PublishDepth 缓冲深度事件（同步、best-effort）。内存发布器无消费方，仅作缓冲。
func (p *InMemPublisher) PublishDepth(ctx context.Context, ev DepthEvent) error {
	p.mu.Lock()
	if len(p.depthBuf) >= p.cap {
		p.depthBuf = p.depthBuf[1:]
	}
	p.depthBuf = append(p.depthBuf, ev)
	p.mu.Unlock()
	return nil
}

// Drain 取出并清空当前成交缓冲（测试/对账用）。
func (p *InMemPublisher) Drain() []TradeEvent {
	p.mu.Lock()
	out := append([]TradeEvent(nil), p.buf...)
	p.buf = nil
	p.mu.Unlock()
	return out
}

// DrainDepth 取出并清空当前深度缓冲（测试用）。
func (p *InMemPublisher) DrainDepth() []DepthEvent {
	p.mu.Lock()
	out := append([]DepthEvent(nil), p.depthBuf...)
	p.depthBuf = nil
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
