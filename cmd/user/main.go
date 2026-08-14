package main

import (
	"flag"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/services/user"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 存储：优先 MySQL（建 ce_ 表并迁移）；失败则降级为内存（重启即丢，仅开发用）。
	var store user.Store
	if cfg.MySQL.DSN != "" {
		store, err = user.NewMySQLStore(cfg.MySQL.DSN)
		if err != nil {
			log.Warn("user: mysql unavailable, fallback to in-memory store", zap.Error(err))
		}
	}
	if store == nil {
		store = user.NewMemStore()
	}

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)
	svc := user.NewService(store, verifier, user.NewLogNotifier(), user.Config{})
	h := user.NewHandler(svc, verifier)

	r := gin.New()
	r.Use(middleware.Common(log, cfg)...)
	h.Register(r)

	addr := ":8081"
	log.Info("user service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
