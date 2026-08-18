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
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/coldlar/crypto-exchange/internal/staking"
)

// cmd/staking 是链上质押理财服务的「装配层」，仅负责：读配置、建账本、选 Store、
// 装配 staking.Service 并注册路由、启动后台奖励归集循环。业务逻辑在 internal/staking。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for staking persistence (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 钱包总账（复式记账），质押本金锁定于 SysStaking、奖励负债记 SysStakingReward。
	ledgerSvc := ledger.New()
	// 演示种子充值：保持账本创世平衡（与 wealth 等一致）。
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = ledgerSvc.ReceiveOnChain(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), fmt.Sprintf("seed:%d:USDT", uid))
		_ = ledgerSvc.ReceiveOnChain(uid, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), fmt.Sprintf("seed:%d:BTC", uid))
	}
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)
	defer ledgerSvc.StopReconciler()

	// 选择 Store：配置了 DSN 则连 MySQL 并跑迁移，否则内存（演示）。
	var store staking.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := staking.NewMySQLStore(dsn)
		if merr != nil {
			log.Warn("staking mysql store unavailable, fallback to in-memory",
				zap.String("dsn", dsn), zap.Error(merr))
			store = staking.NewMemStore()
		} else {
			store = ms
			log.Info("staking store: mysql", zap.String("dsn", dsn))
			defer func() { _ = ms.Close() }()
		}
	} else {
		store = staking.NewMemStore()
		log.Info("staking store: in-memory")
	}

	svc := staking.NewService(store, ledgerSvc, nil, staking.Config{AccrueInterval: 60 * time.Second}, log)

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)
	r := gin.New()
	svc.RegisterRoutes(r, verifier)

	// 演示：若无在售产品，发行一个 ETH 质押示例产品。
	seedDemoProducts(store, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunLoop(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	addr := ":8097"
	log.Info("staking starting", zap.String("addr", addr))
	if err := r.Run(addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}

// seedDemoProducts 在无在售产品时发行一个 ETH 质押示例产品（演示用，绕过 AdminGuard 直接落库）。
func seedDemoProducts(store staking.Store, log *zap.Logger) {
	list, err := store.ListProducts("")
	if err == nil && len(list) > 0 {
		return
	}
	p := &staking.StakingProduct{
		Name:         "ETH 2.0 质押（示例）",
		Chain:        "eth",
		Validator:    "0xDemoValidator",
		ContractAddr: "0xDemoStakingContract",
		Asset:        "ETH",
		AnnualRate:   0.04,
		DurationDays: 0,
		MinAmount:    settlement.AssetAmountFromFloat(0.1, settlement.AssetDecimalsByName("ETH")),
		Status:       staking.ProductActive,
	}
	if err := store.CreateProduct(p); err != nil {
		log.Warn("seed staking product failed", zap.Error(err))
	}
}
