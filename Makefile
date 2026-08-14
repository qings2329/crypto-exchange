# crypto-exchange Makefile
# 构建各微服务二进制到 bin/ 目录

SERVICES := user gateway spot market futures settlement margin notification risk options otc wealth
BIN_DIR := bin

.PHONY: all build clean test lint run-% $(SERVICES)

all: build

build: $(SERVICES)

$(SERVICES):
	@echo "building $@ ..."
	@mkdir -p $(BIN_DIR)
	@go build -o $(BIN_DIR)/$@ ./cmd/$@

run-%:
	@go run ./cmd/$*

test:
	@go test ./...

lint:
	@go vet ./...

# 生成 Protobuf / gRPC Go 代码（需先安装 protoc-gen-go 与 protoc-gen-go-grpc）。
proto:
	@./scripts/gen_proto.sh

clean:
	@rm -rf $(BIN_DIR)
