package main

import (
	"database/sql"
	"flag"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/announcement"
	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/services/user"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	addr := flag.String("addr", ":8081", "listen address")
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
	svc := user.NewService(store, verifier, user.NewLogNotifier(), newUserNotifSvc(cfg.MySQL.DSN, log), user.Config{})
	h := user.NewHandler(svc, verifier)

	// 公告模块：与用户模块共用同一数据库（同一份 ce_schema_migrations，版本号已错开）。
	// 优先 MySQL；DSN 缺失则降级为内存实现（重启即丢，仅开发用）。
	var aStore announcement.Store
	if cfg.MySQL.DSN != "" {
		aStore, err = announcement.NewMySQLStore(cfg.MySQL.DSN)
		if err != nil {
			log.Warn("announcement: mysql unavailable, fallback to in-memory store", zap.Error(err))
		}
	}
	if aStore == nil {
		aStore = announcement.NewMemStore()
	}
	aSvc := announcement.NewService(aStore)
	aH := announcement.NewHandler(aSvc)

	r := gin.New()
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（避免经网关/LB 转发时所有请求被误判为同一上游 IP）。
	middleware.ConfigureTrustedProxies(r, cfg, log)
	r.Use(middleware.Common(log, cfg)...)
	h.Register(r)
	aH.Register(r, verifier)

	log.Info("user service starting", zap.String("addr", *addr))
	if err := cfg.Listen(r, *addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}

// newUserNotifSvc 构造通知服务：优先 MySQL（持久化站内信），失败降级内存。
func newUserNotifSvc(dsn string, log *zap.Logger) *notification.Service {
	if dsn != "" {
		db, err := sql.Open("mysql", dsn)
		if err == nil {
			store, merr := notification.NewMySQLStore(db)
			if merr == nil {
				return notification.New(store)
			}
			log.Warn("user: notification mysql migrate failed, fallback to in-memory", zap.Error(merr))
			_ = db.Close()
		} else {
			log.Warn("user: notification mysql open failed, fallback to in-memory", zap.Error(err))
		}
	}
	return notification.New(notification.NewMemStore())
}
