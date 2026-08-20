package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/lending"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// cmd/lending 是借贷服务的「装配层」，仅负责：读配置、建账本、选 Store、
// 装配 lending.Service 并注册路由、启动后台利息归集循环。业务逻辑在 internal/lending。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for lending persistence (overrides config)")
	addr := flag.String("addr", ":8100", "HTTP listen addr")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 钱包总账（复式记账），借贷本金经 SysLendingPool / SysLendingCollateral 中转。
	ledgerSvc := ledger.New()
	// 演示种子充值：经链上充值（复式记账）预置多资产，使账本从创世起即全局平衡。
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = ledgerSvc.ReceiveOnChain(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), fmt.Sprintf("seed:%d:USDT", uid))
		_ = ledgerSvc.ReceiveOnChain(uid, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), fmt.Sprintf("seed:%d:BTC", uid))
	}
	// 对账巡检：探测不平账并告警。
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)
	defer ledgerSvc.StopReconciler()

	// 选择 Store：配置了 DSN 则连 MySQL 并跑迁移，否则内存（演示）。
	var store lending.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := lending.NewMySQLStore(dsn)
		if merr != nil {
			log.Warn("lending mysql store unavailable, fallback to in-memory",
				zap.String("dsn", dsn), zap.Error(merr))
			store = lending.NewMemStore()
		} else {
			store = ms
			log.Info("lending store: mysql", zap.String("dsn", dsn))
			defer func() { _ = ms.Close() }()
		}
	} else {
		store = lending.NewMemStore()
		log.Info("lending store: in-memory")
	}

	svcCfg := lending.Config{
		AccrueInterval:   60 * time.Second,
		MinBorrowAmount:  10,
		MinLendAmount:    10,
		BaseInterestRate: 0.05,
		MaxInterestRate:  1.0,
	}
	svc := lending.NewService(store, ledgerSvc, svcCfg, log)

	// 演示：若无借贷池，创建一个 USDT 示例池。
	seedDemoPools(store, log)

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)
	r := gin.New()
	middleware.ConfigureTrustedProxies(r, cfg, log)
	r.Use(middleware.Common(log, cfg)...)
	svc.RegisterRoutes(r, verifier)

	// 后台利息归集循环。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunLoop(ctx)

	log.Info("lending starting", zap.String("addr", *addr))

	// 信号退出：先停循环再退出。
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
		_ = log.Sync()
		os.Exit(0)
	}()

	if err := cfg.Listen(r, *addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}

// seedDemoPools 在无借贷池时创建一个 USDT 示例池（演示用）。
func seedDemoPools(store lending.Store, log *zap.Logger) {
	list, err := store.ListPools("")
	if err == nil && len(list) > 0 {
		return
	}
	p := &lending.LendingPool{
		Asset:         "USDT",
		InterestRate:  0.05,
		CollateralReq: 1.5,
		Status:        lending.PoolActive,
		CreatedAt:     time.Now().Unix(),
	}
	if err := store.CreatePool(p); err != nil {
		log.Warn("seed lending pool failed", zap.Error(err))
	}
}
