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

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	r.Use(middleware.Common(log, cfg)...)
	server.RegisterRoutes(r, verifier)

	addr := ":8082"
	log.Info("spot service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
