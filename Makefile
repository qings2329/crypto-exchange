# crypto-exchange Makefile
# 构建各微服务二进制到 bin/ 目录
#
# 构建标签：默认构建不含 Kafka（sarama）依赖，离线可编译可测。
# 生产启用 Kafka 时加 TAGS=kafka，例如：make build TAGS=kafka
# 该标签同时控制 mq 包的 KafkaPublisher/KafkaSubscriber 实现。

SERVICES := user gateway spot market futures settlement margin notification risk options otc wealth
BIN_DIR := bin
TAGS ?=

.PHONY: all build clean test lint run-% $(SERVICES)

all: build

build: $(SERVICES)

$(SERVICES):
	@echo "building $@ ..."
	@mkdir -p $(BIN_DIR)
	@go build -tags=$(TAGS) -o $(BIN_DIR)/$@ ./cmd/$@

run-%:
	@go run -tags=$(TAGS) ./cmd/$*

test:
	@go test -tags=$(TAGS) ./...

lint:
	@go vet -tags=$(TAGS) ./...

# 生成 Protobuf / gRPC Go 代码（需先安装 protoc-gen-go 与 protoc-gen-go-grpc）。
proto:
	@./scripts/gen_proto.sh

clean:
	@rm -rf $(BIN_DIR)
