//go:build !kafka

package mq

// SetKafkaVersion 在默认（无 -tags kafka）构建下为空操作：无 sarama 依赖，
// Publisher/Subscriber 退回内存实现，不需要协议版本协商。
func SetKafkaVersion(string) {}
