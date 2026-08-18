package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"time"

	"github.com/coldlar/crypto-exchange/internal/copytrade"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// cmd/copytrade 是跟单服务的「装配层」：读配置、建账本（仅用于平台复制费结算到
// SysCopyTradeFee）、选 Store、装配 copytrade.Service（代粉丝下单执行器指向 spot/futures、
// 复用其 F1/F4 资金安全）、注册路由、订阅撮合成交流驱动复制。业务逻辑在 internal/copytrade。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for copytrade persistence (overrides config)")
	spotURL := flag.String("spot-url", "http://127.0.0.1:8082", "spot 服务基址（复制下单目标）")
	futuresURL := flag.String("futures-url", "http://127.0.0.1:8084", "futures 服务基址")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// copytrade 自身账本：仅用于平台复制费结算（粉丝 -> SysCopyTradeFee）。
	// 与 staking 同模式——每服务持独立账本，演示种子充值保持创世平衡。
	ledgerSvc := ledger.New()
	for _, uid := range []int64{1, 2, 3, 4, 5, 6} {
		_ = ledgerSvc.ReceiveOnChain(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), fmt.Sprintf("seed:%d:USDT", uid))
	}
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)
	defer ledgerSvc.StopReconciler()

	// 选择 Store：配置了 DSN 则连 MySQL 并跑迁移（9805/9806/9807），否则内存（演示）。
	var store copytrade.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := copytrade.NewMySQLStore(dsn)
		if merr != nil {
			log.Warn("copytrade mysql store unavailable, fallback to in-memory",
				zap.String("dsn", dsn), zap.Error(merr))
			store = copytrade.NewMemStore()
		} else {
			store = ms
			log.Info("copytrade store: mysql", zap.String("dsn", dsn))
			defer func() { _ = ms.Close() }()
		}
	} else {
		store = copytrade.NewMemStore()
		log.Info("copytrade store: in-memory")
	}

	// 代粉丝下单执行器：默认调 spot 的 /order，复用下游 F1(client_oid)/F4(token) 资金安全。
	exec := copytrade.NewHTTPExecutor(*spotURL, *futuresURL)
	svc := copytrade.NewService(store, ledgerSvc, exec, copytrade.Config{MinNotional: 1}, log)

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)
	r := gin.New()
	svc.RegisterRoutes(r, verifier)

	// 订阅撮合成交流：驱动跟单复制。Kafka 构建 + 配置 brokers 时消费真实 exchange.trades；
	// 否则退回内存订阅器（Subscribe 为 no-op，复制不可用，仅 HTTP 管理端点照常）。
	tradeTopic := cfg.Kafka.TradeTopic
	if tradeTopic == "" {
		tradeTopic = "exchange.trades"
	}
	subHandler := func(c context.Context, topic string, data []byte) error {
		if topic != tradeTopic {
			return nil
		}
		var ev mq.TradeEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Warn("copytrade: bad trade payload", zap.Error(err))
			return nil
		}
		svc.OnTrade(c, ev)
		return nil
	}
	subscriber := mq.NewSubscriber(cfg.Kafka.Brokers, "copytrade-replicator", subHandler)
	go func() {
		if err := subscriber.Subscribe(context.Background(), []string{tradeTopic}, subHandler); err != nil && context.Background().Err() == nil {
			log.Warn("copytrade subscriber stopped", zap.Error(err))
		}
	}()
	log.Info("copytrade replicator subscribed", zap.String("topic", tradeTopic))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = subscriber.Close()
		os.Exit(0)
	}()

	addr := ":8099"
	log.Info("copytrade starting", zap.String("addr", addr))
	if err := r.Run(addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
