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
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（避免经网关/LB 转发时所有请求被误判为同一上游 IP）。
	middleware.ConfigureTrustedProxies(r, cfg, log)
	r.Use(middleware.Common(log, cfg)...)
	server.RegisterRoutes(r)

	addr := ":8083"
	log.Info("market service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
