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

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/wealth"
)

// cmd/wealth 是理财资管服务的「装配层」，仅负责：
//   - 读配置、建日志、建账本；
//   - 选择 Store 实现（MySQL + 迁移，失败降级内存）；
//   - 装配 wealth.Service 并注册路由、启动后台应计收益循环。
//
// 所有业务逻辑在 internal/wealth 内，本文件不含业务规则。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for wealth persistence (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 钱包总账（复式记账），用户申购本金与赎回收益均走它（中央托管账户 SysWealth）。
	ledgerSvc := ledger.New()
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = ledgerSvc.Deposit(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed")
		_ = ledgerSvc.Deposit(uid, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), "seed")
	}

	// 选择 Store：配置了 DSN 则连 MySQL 并跑迁移，否则内存（演示）。
	var store wealth.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := wealth.NewMySQLStore(dsn)
		if merr != nil {
			log.Warn("wealth mysql store unavailable, fallback to in-memory",
				zap.String("dsn", dsn), zap.Error(merr))
			store = wealth.NewMemStore()
		} else {
			store = ms
			log.Info("wealth store: mysql", zap.String("dsn", dsn))
			defer func() { _ = ms.Close() }()
		}
	} else {
		store = wealth.NewMemStore()
		log.Info("wealth store: in-memory")
	}

	svcCfg := wealth.Config{AccrueInterval: 60 * time.Second}
	svc := wealth.NewService(store, ledgerSvc, svcCfg, log)

	// 演示：若无任何在售产品，发行一个活期与一个定期示例产品。
	if ps, _ := svc.ListProducts(wealth.ProductOpen); len(ps) == 0 {
		_ = svc.CreateProduct(&wealth.WealthProduct{
			Name: "USDT 活期宝", Asset: "USDT", Type: wealth.TypeCurrent,
			AnnualRate: 0.03, MinAmount: 100,
		})
		_ = svc.CreateProduct(&wealth.WealthProduct{
			Name: "USDT 30 天定期", Asset: "USDT", Type: wealth.TypeFixed,
			AnnualRate: 0.06, DurationDays: 30, MinAmount: 1000,
		})
	}

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	r.Use(middleware.Common(log, cfg)...)
	svc.RegisterRoutes(r, verifier)

	// 后台应计收益循环。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunLoop(ctx)

	addr := ":8092"
	log.Info("wealth service starting", zap.String("addr", addr))

	// 信号退出：先停循环再退出。
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
		svc.Close()
		_ = log.Sync()
		os.Exit(0)
	}()

	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
