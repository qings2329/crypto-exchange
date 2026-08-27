package main

import (
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/ws"
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

	// 通知实时推送：以 user:<id> 为订阅通道复用通用 WebSocket Hub。
	// 后端各业务线（KYC/风控/充值/提现）落库 ce_notifications 后，由下方增量轮询泵
	// 经此 Hub 推送给对应用户已建立的连接，替代前端轮询。
	notifHub := ws.NewHub()
	r.GET("/api/v1/user/notifications/ws", notificationWS(notifHub, verifier))
	go startNotifPump(svc, notifHub, logger)

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

// notificationWS 升级为 WebSocket 并订阅 user:<id> 通道（token 鉴权）。
// 鉴权通过后把订阅 symbol 改写为 user:<id>，复用通用 Hub 的按 symbol 广播能力，
// 使增量轮询泵能精准推送给对应用户。
func notificationWS(hub *ws.Hub, verifier *middleware.TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.Query("token")
		uid, ok := verifier.Verify(token)
		if !ok || uid <= 0 {
			c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"})
			return
		}
		q := c.Request.URL.Query()
		q.Set("symbol", "user:"+strconv.FormatInt(uid, 10))
		c.Request.URL.RawQuery = q.Encode()
		hub.Handle(c.Writer, c.Request)
	}
}

// startNotifPump 增量轮询 ce_notifications，将新增通知经 Hub 实时推送给对应用户。
// 各业务线（user KYC/风控、futures 充值/提现）均落库同一张表，故单一泵即可覆盖全部来源，
// 无需跨进程回调。初始化时跳过历史，仅推送进程启动后新增的通知。
func startNotifPump(svc *notification.Service, hub *ws.Hub, log *zap.Logger) {
	lastID := int64(0)
	if existing, err := svc.ListAll(1); err == nil && len(existing) > 0 {
		lastID = existing[0].ID
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		ns, err := svc.ListSince(int(lastID), 200)
		if err != nil {
			log.Warn("notification pump list failed", zap.Error(err))
			continue
		}
		for _, n := range ns {
			view := gin.H{
				"id":         n.ID,
				"level":      notification.LevelOf(n.Type),
				"title":      n.Title,
				"content":    n.Body,
				"read":       n.Status == notification.StatusRead,
				"created_at": n.CreatedAt,
			}
			hub.Broadcast("user:"+strconv.FormatInt(n.UserID, 10), gin.H{"type": "notification", "data": view})
			if n.ID > lastID {
				lastID = n.ID
			}
		}
	}
}
