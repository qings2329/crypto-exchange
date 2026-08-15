//go:build kafka

// 该文件在 `go build -tags kafka` 时启用（需先 `go get github.com/IBM/sarama`）。
// 默认（无 kafka tag）构建不含 sarama 依赖，Publisher/Subscriber 退回内存实现，保证离线可编译可测。
package mq

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IBM/sarama"
)

func init() {
	kafkaConstructor = func(brokers []string, tradeTopic, depthTopic string) (Publisher, error) {
		return NewKafkaPublisher(brokers, tradeTopic, depthTopic)
	}
	kafkaSubscriberConstructor = func(brokers []string, group string) (Subscriber, error) {
		return NewKafkaSubscriber(brokers, group)
	}
}

// kafkaVersion 是 sarama 与 broker 协商使用的协议版本。
// 注意：sarama.NewConfig() 默认 Version 为最低版本（V0_10_0_0），而 KRaft broker 仅支持
// 较新的协议版本，会导致消费组 JoinGroup/SyncGroup 卡住、永远收不到消息。必须显式设置。
// 默认取 sarama v1.42.1 支持的最高版本（V3_6_0_0）；可用 SetKafkaVersion 按实际 broker
// 覆盖（如 "3.8.0"），sarama 会在请求层向下协商到 broker 实际支持的 API 版本，不影响兼容。
var kafkaVersion = sarama.V3_6_0_0

// SetKafkaVersion 从配置字符串（如 "3.8.0"）覆盖协商版本；空或非法值则保持内置默认。
func SetKafkaVersion(s string) {
	if s == "" {
		return
	}
	if v, err := sarama.ParseKafkaVersion(s); err == nil {
		kafkaVersion = v
	}
}

// KafkaPublisher 将成交流/深度流异步投递到 Kafka（best-effort：失败仅记录，不阻断撮合）。
type KafkaPublisher struct {
	tradeTopic string
	depthTopic string
	producer   sarama.AsyncProducer
}

// NewKafkaPublisher 构造 Kafka 异步生产者（同时持有成交与深度两个 topic）。
func NewKafkaPublisher(brokers []string, tradeTopic, depthTopic string) (*KafkaPublisher, error) {
	cfg := sarama.NewConfig()
	cfg.Version = kafkaVersion
	cfg.Producer.Partitioner = sarama.NewRandomPartitioner
	cfg.Producer.Return.Errors = true
	cfg.Producer.RequiredAcks = sarama.WaitForLocal

	p, err := sarama.NewAsyncProducer(brokers, cfg)
	if err != nil {
		return nil, fmt.Errorf("kafka async producer: %w", err)
	}
	// 后台消费发送错误，避免阻塞且便于观测。
	go func() {
		for err := range p.Errors() {
			fmt.Printf("[mq] kafka publish error: %v\n", err)
		}
	}()
	return &KafkaPublisher{tradeTopic: tradeTopic, depthTopic: depthTopic, producer: p}, nil
}

// PublishTrade 异步投递成交流（JSON 编码）到成交 topic。
func (k *KafkaPublisher) PublishTrade(ctx context.Context, ev TradeEvent) error {
	b, err := ev.JSON()
	if err != nil {
		return err
	}
	k.producer.Input() <- &sarama.ProducerMessage{
		Topic: k.tradeTopic,
		Value: sarama.ByteEncoder(b),
	}
	return nil
}

// PublishDepth 异步投递深度快照（JSON 编码）到深度 topic。
func (k *KafkaPublisher) PublishDepth(ctx context.Context, ev DepthEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	k.producer.Input() <- &sarama.ProducerMessage{
		Topic: k.depthTopic,
		Value: sarama.ByteEncoder(b),
	}
	return nil
}

// Close 关闭生产者。
func (k *KafkaPublisher) Close() error {
	return k.producer.Close()
}

// KafkaSubscriber 以消费组方式订阅 Kafka topic（成交流/深度流）。
type KafkaSubscriber struct {
	brokers []string
	group   string
	client  sarama.ConsumerGroup
}

// NewKafkaSubscriber 构造 Kafka 消费组。
func NewKafkaSubscriber(brokers []string, group string) (*KafkaSubscriber, error) {
	cfg := sarama.NewConfig()
	cfg.Version = kafkaVersion
	cfg.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.BalanceStrategyRange}
	cfg.Consumer.Offsets.Initial = sarama.OffsetNewest
	client, err := sarama.NewConsumerGroup(brokers, group, cfg)
	if err != nil {
		return nil, fmt.Errorf("kafka consumer group: %w", err)
	}
	return &KafkaSubscriber{brokers: brokers, group: group, client: client}, nil
}

// Subscribe 阻塞消费给定 topics，逐条回调 handler；ctx 取消时退出。
func (k *KafkaSubscriber) Subscribe(ctx context.Context, topics []string, handler MsgHandler) error {
	h := &kafkaConsumerGroupHandler{handler: handler}
	for {
		if err := k.client.Consume(ctx, topics, h); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 非 ctx 取消的错误：记录后短暂退避重试。
			fmt.Printf("[mq] kafka consume error: %v\n", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// Close 关闭消费组。
func (k *KafkaSubscriber) Close() error {
	return k.client.Close()
}

// kafkaConsumerGroupHandler 实现 sarama.ConsumerGroupHandler。
type kafkaConsumerGroupHandler struct {
	handler MsgHandler
}

func (h *kafkaConsumerGroupHandler) Setup(_ sarama.ConsumerGroupSession) error  { return nil }
func (h *kafkaConsumerGroupHandler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

func (h *kafkaConsumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		if h.handler != nil {
			if err := h.handler(session.Context(), msg.Topic, msg.Value); err != nil {
				// at-least-once 语义：消费失败仅记录，仍提交位移（调用方需幂等）。
				fmt.Printf("[mq] kafka consume handler error: %v\n", err)
			}
		}
		session.MarkMessage(msg, "")
	}
	return nil
}
