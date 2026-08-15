// 内存订阅器：默认（无 kafka tag）构建下 NewSubscriber 的回退实现。
//
// 它本身不连接任何消息总线；提供 Feed 方法供单测注入消息，驱动 handler。
// 生产环境的回退路径（无 Kafka brokers）由调用方改用 WebSocket 行情流，
// 因此本实现在默认构建下仅供测试使用，Subscribe 为 no-op。

package mq

import (
	"context"
)

// InMemSubscriber 内存订阅器：保存 handler，由 Feed 注入消息。
type InMemSubscriber struct {
	handler MsgHandler
}

// NewInMemSubscriber 构造内存订阅器。
func NewInMemSubscriber(handler MsgHandler) *InMemSubscriber {
	return &InMemSubscriber{handler: handler}
}

// Subscribe 默认构建下为 no-op（内存订阅器无真实连接）。
// 真实订阅由 -tags kafka 构建的 KafkaSubscriber 提供。
func (s *InMemSubscriber) Subscribe(ctx context.Context, topics []string, handler MsgHandler) error {
	return nil
}

// Feed 向订阅器注入一条消息，同步调用 handler（供单测模拟消费）。
func (s *InMemSubscriber) Feed(topic string, data []byte) error {
	if s.handler == nil {
		return nil
	}
	return s.handler(context.Background(), topic, data)
}

// Close 内存订阅器无需释放资源。
func (s *InMemSubscriber) Close() error { return nil }
