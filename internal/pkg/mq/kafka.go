//go:build kafka

// 该文件在 `go build -tags kafka` 时启用（需先 `go get github.com/IBM/sarama`）。
// 默认（无 kafka tag）构建不含 sarama 依赖，Publisher 退回 InMemPublisher，保证离线可编译可测。
package mq

import (
	"context"
	"fmt"

	"github.com/IBM/sarama"
)

func init() {
	kafkaConstructor = func(brokers []string, topic string) (Publisher, error) {
		return NewKafkaPublisher(brokers, topic)
	}
}

// KafkaPublisher 将成交流异步投递到 Kafka（best-effort：失败仅记录，不阻断撮合）。
type KafkaPublisher struct {
	topic    string
	producer sarama.AsyncProducer
}

// NewKafkaPublisher 构造 Kafka 异步生产者。
func NewKafkaPublisher(brokers []string, topic string) (*KafkaPublisher, error) {
	cfg := sarama.NewConfig()
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
	return &KafkaPublisher{topic: topic, producer: p}, nil
}

// PublishTrade 异步投递成交流（JSON 编码）。
func (k *KafkaPublisher) PublishTrade(ctx context.Context, ev TradeEvent) error {
	b, err := ev.JSON()
	if err != nil {
		return err
	}
	k.producer.Input() <- &sarama.ProducerMessage{
		Topic: k.topic,
		Value: sarama.ByteEncoder(b),
	}
	return nil
}

// Close 关闭生产者。
func (k *KafkaPublisher) Close() error {
	return k.producer.Close()
}
