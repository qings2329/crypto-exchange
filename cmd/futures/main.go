package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/futuresapi"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
)

// cmd/futures 是合约交易服务的「装配层」，仅负责：
//   - 读配置、建日志、建账本与消息发布器；
//   - 账本快照的 MySQL 持久化生命周期（恢复 / 种子 / 信号落库 / 退出落库）；
//   - 调用 futuresapi.NewServer 完成业务装配，注册路由并启动 HTTP 服务。
//
// 所有引擎接线、回调、后台循环与 HTTP 路由均在 internal/futuresapi 内，本文件不含业务逻辑。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	// --mysql-dsn：账本持久化快照的 MySQL DSN。指定则覆盖 configs 中的 mysql.dsn。
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for ledger snapshot persistence (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 钱包总账（复式记账），承载保证金冻结、资金费结算与强平没收。
	ledgerSvc := ledger.New()

	// 成交流发布器：撮合成交后发布到消息总线（配置了 Kafka brokers 且 -tags kafka
	// 构建时投递 Kafka，否则内存降级），供清算/记账服务消费。
	publisher := mq.NewPublisher(cfg.Kafka.Brokers, cfg.Kafka.TradeTopic, nil)
	defer publisher.Close()

	// 持久化 DSN：优先 --mysql-dsn，否则取 config.MySQL.DSN。
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}

	// 演示种子充值：为常用用户预置 USDT 余额（生产来自链上充值/清结算）。
	seedDemo := func() {
		for _, uid := range []int64{1, 2, 3, 4} {
			_ = ledgerSvc.Deposit(uid, "USDT", 100000, "seed")
		}
	}

	// 持久化恢复：若配置了 MySQL DSN，优先从库加载——跨进程生命周期保留坏账限制、
	// 余额、治理提案与风控事件；加载成功跳过种子充值。库无快照行或连接失败均回退到种子。
	if dsn != "" {
		if snap, ok, lerr := ledger.LoadSnapshotFromMySQL(dsn, "futures"); lerr == nil {
			if ok {
				ledgerSvc.Restore(snap)
				log.Info("ledger state restored from mysql", zap.String("dsn", dsn),
					zap.Int("accounts", len(snap.Accounts)), zap.Int("entries", len(snap.Log)))
			} else {
				log.Info("no ledger snapshot in mysql, seeding demo", zap.String("dsn", dsn))
				seedDemo()
			}
		} else {
			log.Warn("ledger mysql load failed, falling back to seed deposit",
				zap.String("dsn", dsn), zap.Error(lerr))
			seedDemo()
		}
	} else {
		seedDemo()
	}

	// 装配合约交易服务（引擎/预言机/网关/资金费循环/账本风控），不含业务逻辑。
	// matchingURL 指向 cmd/matching 服务，撮合收敛为单一权威（见 DEVELOPMENT_TASKS §18）。
	server := futuresapi.NewServer(ledgerSvc, publisher, log, dsn, cfg.Matching.URL)
	defer server.Close()

	// 进程退出前持久化账本状态到 MySQL（正常返回或 Ctrl+C/kill 触发），保证资金安全态不丢失。
	// 配置了 DSN 才落库；未配置则跳过（纯内存演示）。Go 的 defer 在收到信号被终止时不会执行，
	// 故显式监听信号完成"退出前持久化"。
	if dsn != "" {
		defer func() {
			if serr := ledgerSvc.SaveToMySQL(dsn, "futures"); serr != nil {
				log.Error("ledger snapshot save to mysql on shutdown failed", zap.Error(serr))
			} else {
				log.Info("ledger state saved to mysql", zap.String("dsn", dsn))
			}
		}()
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			server.Close()
			if serr := ledgerSvc.SaveToMySQL(dsn, "futures"); serr != nil {
				log.Error("ledger snapshot save to mysql on signal failed", zap.Error(serr))
			} else {
				log.Info("ledger state saved to mysql on signal", zap.String("dsn", dsn))
			}
			_ = log.Sync()
			os.Exit(0)
		}()
	}

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)

	r := gin.New()
	r.Use(middleware.Common(log, cfg)...)
	server.RegisterRoutes(r, verifier)

	addr := ":8084"
	log.Info("futures service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
