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
	"github.com/coldlar/crypto-exchange/internal/margin"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/coldlar/crypto-exchange/internal/oracle"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// cmd/margin 是现货杠杆服务的「装配层」，仅负责：
//   - 读配置、建日志、建账本与价格预言机；
//   - 选择 Store 实现（MySQL + 迁移，失败降级内存）；
//   - 装配 margin.Service 并注册路由、启动后台计息/强平循环。
//
// 所有业务逻辑在 internal/margin 内，本文件不含业务规则。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for margin persistence (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 钱包总账（复式记账），杠杆的抵押冻结、贷出资产与罚没均走它。
	ledgerSvc := ledger.New()
	// 演示种子充值：经链上充值（复式记账）预置 USDT，使账本从创世起即全局平衡（对账巡检不误报）。
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = ledgerSvc.ReceiveOnChain(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), fmt.Sprintf("seed:%d:USDT", uid))
	}
	// 对账巡检：探测不平账并告警（演示日志钩子）。
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)
	defer ledgerSvc.StopReconciler()

	// 选择 Store：配置了 DSN 则连 MySQL 并跑迁移，否则内存（演示）。
	var store margin.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := margin.NewMySQLStore(dsn)
		if merr != nil {
			log.Warn("margin mysql store unavailable, fallback to in-memory",
				zap.String("dsn", dsn), zap.Error(merr))
			store = margin.NewMemStore()
		} else {
			store = ms
			log.Info("margin store: mysql", zap.String("dsn", dsn))
			defer func() { _ = ms.Close() }()
		}
	} else {
		store = margin.NewMemStore()
		log.Info("margin store: in-memory")
	}

	// 价格预言机：取借入资产标记价用于强平价评估。无配置源时用空配置（无价格，强平跳过）。
	oracleSvc := oracle.NewFromConfig(cfg.Oracle)
	oracleSvc.Start()
	defer oracleSvc.Stop()
	priceFn := func(asset string) (float64, bool) {
		return oracleSvc.IndexPrice(asset + "_USDT")
	}

	svcCfg := margin.Config{
		MaxLeverage:      5,
		HourlyRate:       0.0001, // 0.01%/h
		MaintenanceRatio: 1.05,
		CollateralAsset:  "USDT",
		AccrueInterval:   30 * time.Second,
		LiquidationPenalty: 0.05,
	}
	svc := margin.NewService(store, ledgerSvc, svcCfg, log, priceFn)

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（避免经网关/LB 转发时所有请求被误判为同一上游 IP）。
	middleware.ConfigureTrustedProxies(r, cfg, log)
	r.Use(middleware.Common(log, cfg)...)
	svc.RegisterRoutes(r, verifier)

	// 后台计息 + 自动强平循环。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunLoop(ctx)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	if cfg.Server.Port == 0 {
		addr = ":8087"
	}
	log.Info("margin service starting", zap.String("addr", addr))

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
