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

	"github.com/coldlar/crypto-exchange/internal/earn"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// cmd/earn 是理财中心（Earn Hub）+ 新币挖矿（Launchpool）服务的「装配层」，仅负责：
//   - 读配置、建日志、建账本；
//   - 选择 Store 实现（MySQL + 迁移，失败降级内存）；
//   - 装配 earn.Service 并注册路由、启动后台计息循环。
//
// 所有业务逻辑在 internal/earn 内，本文件不含业务规则。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for earn persistence (overrides config)")
	addr := flag.String("addr", ":8093", "listen address")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 钱包总账（复式记账）：申购本金走 SysWealth 托管，收益经 SysWealthYieldPayable；
	// Launchpool 质押本金走 SysStaking，新币奖励预算经 SysStakingReward 池支出。
	ledgerSvc := ledger.New()
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = ledgerSvc.ReceiveOnChain(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), fmt.Sprintf("seed:%d:USDT", uid))
		_ = ledgerSvc.ReceiveOnChain(uid, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), fmt.Sprintf("seed:%d:BTC", uid))
		_ = ledgerSvc.ReceiveOnChain(uid, "ETH", settlement.AssetAmountFromFloat(100, settlement.AssetDecimalsByName("ETH")), fmt.Sprintf("seed:%d:ETH", uid))
	}
	// 对账巡检：探测不平账并告警。
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)
	defer ledgerSvc.StopReconciler()

	var store earn.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := earn.NewMySQLStore(dsn)
		if merr != nil {
			log.Warn("earn mysql store unavailable, fallback to in-memory",
				zap.String("dsn", dsn), zap.Error(merr))
			store = earn.NewMemStore()
		} else {
			store = ms
			log.Info("earn store: mysql", zap.String("dsn", dsn))
			defer func() { _ = ms.Close() }()
		}
	} else {
		store = earn.NewMemStore()
		log.Info("earn store: in-memory")
	}

	svcCfg := earn.Config{AccrueInterval: 60 * time.Second}
	svc := earn.NewService(store, ledgerSvc, svcCfg, log)

	// 演示种子：无产品时发行一个活期与一个定期示例；无项目时发行一个进行中的 Launchpool 示例
	// （管理员 uid=1 自带 token 预算充值，演示奖励可领）。
	if ps, _ := svc.ListProducts(""); len(ps) == 0 {
		_ = svc.CreateProduct(&earn.EarnProduct{
			Name: "USDT 活期理财", Asset: "USDT", TermDays: 0,
			APY: 0.04, MinAmount: 10,
		})
		_ = svc.CreateProduct(&earn.EarnProduct{
			Name: "USDT 30 天定期", Asset: "USDT", TermDays: 30,
			APY: 0.08, MinAmount: 100, MaxAmount: 50000,
		})
	}
	if projects, _ := svc.ListProjects(); len(projects) == 0 {
		now := time.Now()
		p := &earn.LaunchProject{
			Name: "NEW 代币挖矿", Token: "NEW", TotalSupply: "100000000",
			StartsAt: now.Add(-time.Hour), EndsAt: now.Add(30 * 24 * time.Hour),
			Pools: []earn.LaunchPool{
				{ID: "usdt", Asset: "USDT", APY: 0.15},
				{ID: "btc", Asset: "BTC", APY: 0.05},
			},
		}
		if err := svc.CreateProject(p); err == nil {
			// 幂等键使重复启动不会重复扣款（F1）
			_, _ = svc.FundProject(1, p.ID, 1_000_000, "seed-launchpool-new")
		} else {
			log.Warn("seed launch project failed", zap.Error(err))
		}
	}

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	middleware.ConfigureTrustedProxies(r, cfg, log)
	r.Use(middleware.Common(log, cfg)...)
	svc.RegisterRoutes(r, verifier)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunLoop(ctx)

	log.Info("earn service starting", zap.String("addr", *addr))

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
