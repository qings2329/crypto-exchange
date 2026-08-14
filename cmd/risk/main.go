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

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/risk"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	logger, _ := zap.NewProduction()
	defer logger.Sync()
	gin.SetMode(gin.ReleaseMode)

	// 装配存储：优先 MySQL（建 ce_ 表），失败降级内存。
	var store risk.Store
	if cfg.MySQL.DSN != "" {
		db, derr := sql.Open("mysql", cfg.MySQL.DSN)
		if derr == nil {
			if perr := db.Ping(); perr == nil {
				if ms, merr := risk.NewMySQLStore(db); merr == nil {
					store = ms
					logger.Info("risk store: mysql")
				} else {
					logger.Warn("risk mysql migrate failed, fallback to mem", zap.Error(merr))
					_ = db.Close()
				}
			} else {
				logger.Warn("risk mysql ping failed, fallback to mem", zap.Error(perr))
				_ = db.Close()
			}
		} else {
			logger.Warn("risk sql.Open failed, fallback to mem", zap.Error(derr))
		}
	}
	if store == nil {
		store = risk.NewMemStore()
		logger.Info("risk store: in-memory (no MySQL)")
	}

	svc := risk.New(store)
	h := risk.NewHandler(svc)
	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	mws := middleware.Common(logger, cfg)
	r.Use(append(mws, middleware.Auth(verifier))...)
	h.RegisterRoutes(r)

	addr := ":8089"
	srv := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("risk service listening", zap.String("addr", addr))
		if err := cfg.ListenServer(srv); err != nil && err != http.ErrServerClosed {
			logger.Fatal("risk serve", zap.Error(err))
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("risk service shutting down")
}
