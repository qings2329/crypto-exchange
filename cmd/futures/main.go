package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/catalog"
	"github.com/coldlar/crypto-exchange/internal/futuresapi"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/risk"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// cmd/futures 是合约交易服务的「装配层」，仅负责：
//   - 读配置、建日志、建账本；
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
	// 对账巡检：探测不平账并告警。与其余有账本服务一致——futures 是资金核心服务，
	// 此前漏接此接线，账本不平衡不会被后台巡检发现（已对齐 staking/margin/otc 等）。
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)
	defer ledgerSvc.StopReconciler()

	// 持久化 DSN：优先 --mysql-dsn，否则取 config.MySQL.DSN。
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}

	// seedWithdrawHoldsDemo 预置若干提现 hold：先登记「已验证且已过验证期」的地址，再发起提现。
	// 金额、地址均为演示假数据（纯内存部署专用）。
	seedWithdrawHoldsDemo := func() {
		usdt := settlement.AssetDecimalsByName("USDT")
		type wh struct {
			uid   int64
			amt   float64
			addr  string
			chain string
		}
		demo := []wh{
			{1, 150000, "TSeededWithdrawAddrUser100000000001", "TRON"},
			{2, 45000, "0xSeededWithdrawAddrUser200000000002", "ETH"},
			{3, 30000, "TSeededWithdrawAddrUser300000000003", "TRON"},
			{4, 60000, "1SeededWithdrawAddrUser40000000004", "BTC"},
		}
		for _, d := range demo {
			ledgerSvc.SeedVerifiedWithdrawAddress(d.uid, "USDT", d.chain, d.addr)
			if _, _, err := ledgerSvc.RequestWithdrawHold(
				d.uid, "USDT",
				settlement.AssetAmountFromFloat(d.amt, usdt),
				settlement.AssetAmountFromFloat(5, usdt),
				d.chain, d.addr,
			); err != nil {
				log.Warn("seed withdraw hold failed", zap.Int64("uid", d.uid), zap.Error(err))
			}
		}
	}

	// 演示种子充值：经链上充值（复式记账）预置 USDT，使账本从创世起即全局平衡（对账巡检不误报）。
	// 同时登记已验证提现地址并发起若干演示提现 hold，供管理后台「充提币记录 / 大额提现人工审核」展示测试。
	seedDemo := func() {
		for _, uid := range []int64{1, 2, 3, 4} {
			_ = ledgerSvc.ReceiveOnChain(uid, "USDT", settlement.AssetAmountFromFloat(300000, settlement.AssetDecimalsByName("USDT")), fmt.Sprintf("seed:%d:USDT", uid))
		}
		seedWithdrawHoldsDemo()
	}

	// 种子标记：仅在纯内存/无快照路径播种（含账户充值、提现 hold 与链上充值网关挂起单）。
	seeded := false

	// 持久化恢复：若配置了 MySQL DSN，优先从库加载——跨进程生命周期保留坏账限制、
	// 余额、治理提案与风控事件；加载成功跳过种子充值。库无快照行或连接失败均回退到种子。
	// 演示种子会在账本预置充值（经链上充值真实入账）。生产环境（配置了 MySQL DSN）
	// 仅在显式设置 FUTURES_DEMO_SEED=1 时才播种，避免清空/误连空库时意外对账户入账。
	// 纯内存模式（无 DSN）仍默认播种，便于本地开发演示。
	demoSeed := os.Getenv("FUTURES_DEMO_SEED") == "1"
	if dsn != "" {
		if snap, ok, lerr := ledger.LoadSnapshotFromMySQL(dsn, "futures"); lerr == nil {
			if ok {
				ledgerSvc.Restore(snap)
				log.Info("ledger state restored from mysql", zap.String("dsn", dsn),
					zap.Int("accounts", len(snap.Accounts)), zap.Int("entries", len(snap.Log)))
			} else if demoSeed {
				log.Info("no ledger snapshot in mysql, seeding demo (FUTURES_DEMO_SEED=1)", zap.String("dsn", dsn))
				seedDemo()
				seeded = true
			} else {
				log.Warn("no ledger snapshot in mysql and FUTURES_DEMO_SEED not set; skipping demo seed to avoid unintended on-chain credit", zap.String("dsn", dsn))
			}
		} else if demoSeed {
			log.Warn("ledger mysql load failed, falling back to seed deposit (FUTURES_DEMO_SEED=1)",
				zap.String("dsn", dsn), zap.Error(lerr))
			seedDemo()
			seeded = true
		} else {
			log.Warn("ledger mysql load failed and FUTURES_DEMO_SEED not set; skipping demo seed", zap.String("dsn", dsn), zap.Error(lerr))
		}
	} else {
		seedDemo()
		seeded = true
	}

	// 风控服务：与 cmd/risk 共享同一 MySQL 的 ce_risk_* 表（进程内依赖 risk 库），
	// 接入提现强制路径前先校验黑名单/限额/KYC/负金额。MySQL 不可用时降级内存（仅演示）。
	var riskSvc *risk.Service
	if dsn != "" {
		if db, derr := sql.Open("mysql", dsn); derr == nil {
			if perr := db.Ping(); perr == nil {
				if ms, merr := risk.NewMySQLStore(db); merr == nil {
					riskSvc = risk.New(ms)
					log.Info("risk store: mysql")
					// 账本操作级幂等指纹持久化（#26）：与风控共享同一 *sql.DB，
					// 重启后同 ref 的重复转账/冻结可被检测并跳过，防双付。
					if ierr := ledgerSvc.SetIdempotencyDB(db, "futures"); ierr != nil {
						log.Warn("ledger idempotency db init failed", zap.Error(ierr))
					} else {
						log.Info("ledger idempotency: mysql")
					}
				} else {
					log.Warn("risk mysql migrate failed, fallback to mem", zap.Error(merr))
					_ = db.Close()
				}
			} else {
				log.Warn("risk mysql ping failed, fallback to mem", zap.Error(perr))
				_ = db.Close()
			}
		} else {
			log.Warn("risk sql.Open failed, fallback to mem", zap.Error(derr))
		}
	}
	if riskSvc == nil {
		riskSvc = risk.New(risk.NewMemStore())
		log.Info("risk store: in-memory (no MySQL)")
	}

	// 链上 RPC 端点以数据库 ce_admin_chains.rpc_endpoint 为单一数据源；无 DSN 的纯内存
	// 演示模式才回退到 config.yaml / 环境变量的 settlement.chain_rpc.endpoints。
	if dsn != "" {
		if rdb, rderr := sql.Open("mysql", dsn); rderr == nil {
			if rpcMap, rerr := catalog.LoadChainRPCEndpoints(rdb); rerr == nil {
				// 管理后台目录里 Tron 的 symbol 记为 "TRX"，而 settlement 的 Chain 键为 "TRON"，
				// 此处对齐，避免 TRON 的 RPC 端点被漏配。
				alias := map[string]string{"TRX": "TRON"}
				if cfg.Settlement.ChainRPC.Endpoints == nil {
					cfg.Settlement.ChainRPC.Endpoints = map[string]string{}
				}
				for sym, url := range rpcMap {
					key := sym
					if k, ok := alias[sym]; ok {
						key = k
					}
					cfg.Settlement.ChainRPC.Endpoints[key] = url
				}
				log.Info("rpc endpoints loaded from catalog db", zap.Int("count", len(rpcMap)))
			} else {
				log.Warn("load rpc endpoints from db failed, fallback to config", zap.Error(rerr))
			}
			_ = rdb.Close()
		}
	}

	// 装配合约交易服务（引擎/预言机/网关/资金费循环/账本风控），不含业务逻辑。
	// matchingURL 指向 cmd/matching 服务，撮合收敛为单一权威（见 DEVELOPMENT_TASKS §18）。
	server := futuresapi.NewServer(ledgerSvc, log, cfg, dsn, cfg.Matching.URL, cfg.Oracle, cfg.Settlement.ChainRPC, riskSvc, cfg.Services["user"])
	defer server.Close()

	// 演示充值记录：链上充值网关为内存挂起单（不参与账本快照持久化），仅在种子路径注入。
	if seeded {
		server.SeedDemoDeposits()
	}

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
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（避免经网关/LB 转发时所有请求被误判为同一上游 IP）。
	middleware.ConfigureTrustedProxies(r, cfg, log)
	r.Use(middleware.Common(log, cfg)...)
	server.RegisterRoutes(r, verifier)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	if cfg.Server.Port == 0 {
		addr = ":8084"
	}
	log.Info("futures service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
