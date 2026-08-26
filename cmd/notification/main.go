package main

import (
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	addr := flag.String("addr", ":8088", "listen address")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	logger, _ := zap.NewProduction()
	defer logger.Sync()
	gin.SetMode(gin.ReleaseMode)

	// 装配存储：优先 MySQL（建 ce_ 表），失败降级内存。
	var store notification.Store
	if cfg.MySQL.DSN != "" {
		db, derr := sql.Open("mysql", cfg.MySQL.DSN)
		if derr == nil {
			if perr := db.Ping(); perr == nil {
				if ms, merr := notification.NewMySQLStore(db); merr == nil {
					store = ms
					logger.Info("notification store: mysql")
				} else {
					logger.Warn("notification mysql migrate failed, fallback to mem", zap.Error(merr))
					_ = db.Close()
				}
			} else {
				logger.Warn("notification mysql ping failed, fallback to mem", zap.Error(perr))
				_ = db.Close()
			}
		} else {
			logger.Warn("notification sql.Open failed, fallback to mem", zap.Error(derr))
		}
	}
	if store == nil {
		store = notification.NewMemStore()
		logger.Info("notification store: in-memory (no MySQL)")
	}

	svc := notification.New(store)
	h := notification.NewHandler(svc)
	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（避免经网关/LB 转发时所有请求被误判为同一上游 IP）。
	middleware.ConfigureTrustedProxies(r, cfg, logger)
	mws := middleware.Common(logger, cfg)
	r.Use(append(mws, middleware.Auth(verifier))...)
	h.RegisterRoutes(r)

	srv := &http.Server{Addr: *addr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("notification service listening", zap.String("addr", *addr))
		if err := cfg.ListenServer(srv); err != nil && err != http.ErrServerClosed {
			logger.Fatal("notification serve", zap.Error(err))
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("notification service shutting down")
}
