package main

import (
	"flag"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/market"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
)

// cmd/market 是行情服务的「装配层」，仅负责读配置、建日志、调用 market.NewServer
// 完成业务装配，注册路由并启动 HTTP 服务。行情存储、WebSocket、演示喂价与路由均在
// internal/market 内，本文件不含业务逻辑。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 按配置覆盖 Kafka 协议协商版本（无 -tags kafka 时为空操作）。
	mq.SetKafkaVersion(cfg.Kafka.Version)

	server := market.NewServer(cfg, log)
	defer server.Close()

	r := gin.New()
	r.Use(middleware.Common(log, cfg)...)
	server.RegisterRoutes(r)

	addr := ":8083"
	log.Info("market service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
