package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/spot"
)

// cmd/spot 是现货交易服务的「装配层」，仅负责读配置、建日志、调用 spot.NewServer
// 完成业务装配，注册路由并启动 HTTP 服务。撮合引擎接线与路由均在 internal/spot 内，
// 本文件不含业务逻辑。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for spot order persistence (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 钱包总账（复式记账）：承接现货下单预冻结、成交结算与对账。现货与合约各自拥有
	// 独立的进程内账本实例（演示架构；长期应抽为共享账本服务）。
	ledgerSvc := ledger.New()

	// 演示种子充值：通过链上充值（复式记账）为常用用户预置多资产余额，供现货买卖与结算。
	// 用 ReceiveOnChain 而非 Deposit，使账本从创世起即全局平衡（对账巡检不会误报）。
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = ledgerSvc.ReceiveOnChain(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), fmt.Sprintf("seed:%d:USDT", uid))
		_ = ledgerSvc.ReceiveOnChain(uid, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), fmt.Sprintf("seed:%d:BTC", uid))
		_ = ledgerSvc.ReceiveOnChain(uid, "ETH", settlement.AssetAmountFromFloat(100, settlement.AssetDecimalsByName("ETH")), fmt.Sprintf("seed:%d:ETH", uid))
	}
	// 对账巡检：探测不平账并告警（演示日志钩子）。
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)
	defer ledgerSvc.StopReconciler()

	server := spot.NewServer(ledgerSvc, cfg, log)
	defer server.Close()

	// 订单持久化：配置了 DSN 则连 MySQL 并跑迁移，重启后据此恢复 clientOIDMap（防重启+重试双冻）；
	// 否则纯内存（演示），重启间隙幂等映射清零。
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		if spotStore, serr := spot.NewMySQLStore(dsn); serr != nil {
			log.Warn("spot mysql store unavailable, fallback to in-memory (restart idempotency unprotected)",
				zap.String("dsn", dsn), zap.Error(serr))
		} else {
			server.SetStore(spotStore)
			if recs, lerr := spotStore.LoadOrders(); lerr != nil {
				log.Warn("spot load orders failed", zap.Error(lerr))
			} else if len(recs) > 0 {
				server.RestoreOrders(recs)
				log.Info("spot restored idempotency map from persisted orders", zap.Int("orders", len(recs)))
			}
		}
	}

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（避免经网关/LB 转发时所有请求被误判为同一上游 IP）。
	middleware.ConfigureTrustedProxies(r, cfg, log)
	r.Use(middleware.Common(log, cfg)...)
	server.RegisterRoutes(r, verifier)

	addr := ":8082"
	log.Info("spot service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
