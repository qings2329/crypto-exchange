package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/bot"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// cmd/bot 是交易机器人服务的「装配层」：读配置、选 Store、装配 bot.Service（下单执行器指向
// spot/futures 后端，复用其资金预冻/结算安全层与 F1/F4 守卫）、注册路由、启动后台 tick 循环。
// 业务逻辑在 internal/bot。bot 本身不持有账本——资金安全下沉到被代理的 spot/futures 服务。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for bot persistence (overrides config)")
	spotURL := flag.String("spot-url", "http://127.0.0.1:8082", "spot 服务基址（bot 下单目标）")
	futuresURL := flag.String("futures-url", "http://127.0.0.1:8084", "futures 服务基址（bot 下单目标）")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 选择 Store：配置了 DSN 则连 MySQL 并跑迁移（9803/9804），否则内存（演示）。
	var store bot.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := bot.NewMySQLStore(dsn)
		if merr != nil {
			log.Warn("bot mysql store unavailable, fallback to in-memory",
				zap.String("dsn", dsn), zap.Error(merr))
			store = bot.NewMemStore()
		} else {
			store = ms
			log.Info("bot store: mysql", zap.String("dsn", dsn))
			defer func() { _ = ms.Close() }()
		}
	} else {
		store = bot.NewMemStore()
		log.Info("bot store: in-memory")
	}

	// 下单执行器：默认调 spot/futures 的 /order，复用下游 F1(client_oid)/F4(token) 资金安全。
	exec := bot.NewHTTPExecutor(*spotURL, *futuresURL)
	svc := bot.NewService(store, nil, exec, bot.Config{TickInterval: 10 * time.Second}, log)

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)
	r := gin.New()
	svc.RegisterRoutes(r, verifier)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	addr := ":8098"
	log.Info("bot starting", zap.String("addr", addr))
	if err := r.Run(addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
