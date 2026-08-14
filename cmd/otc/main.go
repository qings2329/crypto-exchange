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
	"github.com/coldlar/crypto-exchange/internal/oracle"
	"github.com/coldlar/crypto-exchange/internal/otc"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// cmd/otc 是场外交易服务的「装配层」，仅负责：
//   - 读配置、建日志、建账本与价格预言机；
//   - 选择 Store 实现（MySQL + 迁移，失败降级内存）；
//   - 装配 otc.Service 并注册路由、启动后台对账循环。
//
// 所有业务逻辑在 internal/otc 内，本文件不含业务规则。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for otc persistence (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 钱包总账（复式记账），OTC 成交的 crypto 托管/释放均走它（中央托管账户 SysOtc）。
	ledgerSvc := ledger.New()
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = ledgerSvc.Deposit(uid, "BTC", 10, "seed")  // 演示用加密资产
		_ = ledgerSvc.Deposit(uid, "USDT", 100000, "seed")
	}

	// 选择 Store：配置了 DSN 则连 MySQL 并跑迁移，否则内存（演示）。
	var store otc.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := otc.NewMySQLStore(dsn)
		if merr != nil {
			log.Warn("otc mysql store unavailable, fallback to in-memory",
				zap.String("dsn", dsn), zap.Error(merr))
			store = otc.NewMemStore()
		} else {
			store = ms
			log.Info("otc store: mysql", zap.String("dsn", dsn))
			defer func() { _ = ms.Close() }()
		}
	} else {
		store = otc.NewMemStore()
		log.Info("otc store: in-memory")
	}

	// 价格预言机：OTC 核心流程不强制依赖行情，预留扩展入口（返回 (0,false) 即无价）。
	oracleSvc := oracle.New(oracle.Config{})
	oracleSvc.Start()
	defer oracleSvc.Stop()
	priceFn := func(asset string) (float64, bool) {
		return oracleSvc.IndexPrice(asset + "_USDT")
	}

	svcCfg := otc.Config{
		Asset:             "BTC",
		ReconcileInterval: 60 * time.Second,
	}
	svc := otc.NewService(store, ledgerSvc, svcCfg, log, priceFn)

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	r.Use(middleware.Common(log, cfg)...)
	svc.RegisterRoutes(r, verifier)

	// 后台对账循环。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunLoop(ctx)

	addr := ":8091"
	log.Info("otc service starting", zap.String("addr", addr))

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
