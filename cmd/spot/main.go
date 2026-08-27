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
	addr := flag.String("addr", ":8082", "HTTP listen addr")
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
	// 对账巡检：探测不平账并告警（演示日志钩子）。
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

	// 演示种子充值：通过链上充值（复式记账）为常用用户预置多资产余额，供现货买卖与结算。
	// 用 ReceiveOnChain 而非 Deposit，使账本从创世起即全局平衡（对账巡检不会误报）。
	seedDemo := func() {
		for _, uid := range []int64{1, 2, 3, 4} {
			_ = ledgerSvc.ReceiveOnChain(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), fmt.Sprintf("seed:%d:USDT", uid))
			_ = ledgerSvc.ReceiveOnChain(uid, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), fmt.Sprintf("seed:%d:BTC", uid))
			_ = ledgerSvc.ReceiveOnChain(uid, "ETH", settlement.AssetAmountFromFloat(100, settlement.AssetDecimalsByName("ETH")), fmt.Sprintf("seed:%d:ETH", uid))
		}
	}

	// 账本快照持久化（与 cmd/futures 同方案）：配置了 DSN 则启动时优先从 MySQL 恢复——
	// 余额/冻结跨重启保留，这是 openOrders 安全重建的前置条件；库无快照或加载失败均回退种子。
	if dsn != "" {
		if snap, ok, lerr := ledger.LoadSnapshotFromMySQL(dsn, "spot"); lerr == nil {
			if ok {
				ledgerSvc.Restore(snap)
				log.Info("spot ledger restored from mysql", zap.String("dsn", dsn),
					zap.Int("accounts", len(snap.Accounts)), zap.Int("entries", len(snap.Log)))
			} else {
				log.Info("no spot ledger snapshot in mysql, seeding demo", zap.String("dsn", dsn))
				seedDemo()
			}
		} else {
			log.Warn("spot ledger mysql load failed, falling back to seed deposit",
				zap.String("dsn", dsn), zap.Error(lerr))
			seedDemo()
		}
	} else {
		seedDemo()
	}

	server := spot.NewServer(ledgerSvc, cfg, log)
	defer server.Close()

	// 订单持久化：配置了 DSN 则连 MySQL 并跑迁移，重启后据此：
	//   1) 账本幂等指纹持久化（防重启双付，是补账重放的安全前提）；
	//   2) 重启补账（重放停机窗口内漏掉的成交）；
	//   3) 经 RestoreOrders 与撮合对账重建 clientOIDMap/openOrders。
	// 否则纯内存（演示），重启间隙幂等映射清零、openOrders 丢失。
	if dsn != "" {
		var (
			pending []spot.OrderRecord
			storeOK bool
		)
		if spotStore, serr := spot.NewMySQLStore(dsn); serr != nil {
			log.Warn("spot mysql store unavailable, fallback to in-memory (restart idempotency unprotected)",
				zap.String("dsn", dsn), zap.Error(serr))
		} else {
			server.SetStore(spotStore)
			storeOK = true
			if recs, lerr := spotStore.LoadOrders(); lerr != nil {
				log.Warn("spot load orders failed", zap.Error(lerr))
			} else {
				pending = recs
			}
		}
		// 账本操作级幂等指纹持久化（#26）：重启后同 ref 的重复转账被检测并静默跳过，
		// 是 CatchUpSettlement 重放补账防双付的前提。
		idempotencyOK := false
		if db, derr := sql.Open("mysql", dsn); derr == nil {
			if perr := db.Ping(); perr == nil {
				if ierr := ledgerSvc.SetIdempotencyDB(db, "spot"); ierr != nil {
					log.Warn("spot ledger idempotency db init failed", zap.Error(ierr))
				} else {
					idempotencyOK = true
					log.Info("spot ledger idempotency: mysql")
				}
			} else {
				log.Warn("spot idempotency db ping failed", zap.Error(perr))
				_ = db.Close()
			}
		}
		// 重启补账：必须先于 RestoreOrders——此刻 openOrders 尚空，settleFill 走「无冻结记录→
		// 纯转账」分支，所有操作均携带成交 ref、受幂等指纹保护不会双付。若先恢复冻结登记，
		// 重放旧成交会触发无 ref 的 Unfreeze（不受指纹保护）造成二次解冻。
		// 未接幂等库则跳过补账（无法区分已结/未结），依赖 reconcile 告警人工介入。
		if idempotencyOK {
			server.CatchUpSettlement()
		} else {
			log.Warn("spot catchup settlement skipped (idempotency db unavailable)")
		}
		// 冻结登记恢复：与撮合在簿状态对账（终态单释放残留冻结并清理记录，见 store.go）。
		if storeOK && len(pending) > 0 {
			server.RestoreOrders(pending)
			log.Info("spot restored freeze registry from persisted orders", zap.Int("orders", len(pending)))
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

	// 进程退出前持久化账本状态到 MySQL（正常返回或 Ctrl+C/kill 触发），保证余额/冻结
	// 不丢失——这是重启后 openOrders 安全重建的数据基础。Go 的 defer 在收到信号被终止时
	// 不会执行，故显式监听信号完成「退出前持久化」。未配置 DSN 则跳过（纯内存演示）。
	if dsn != "" {
		defer func() {
			if serr := ledgerSvc.SaveToMySQL(dsn, "spot"); serr != nil {
				log.Error("spot ledger snapshot save to mysql on shutdown failed", zap.Error(serr))
			} else {
				log.Info("spot ledger state saved to mysql", zap.String("dsn", dsn))
			}
		}()
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			server.Close()
			if serr := ledgerSvc.SaveToMySQL(dsn, "spot"); serr != nil {
				log.Error("spot ledger snapshot save to mysql on signal failed", zap.Error(serr))
			} else {
				log.Info("spot ledger state saved to mysql on signal", zap.String("dsn", dsn))
			}
			_ = log.Sync()
			os.Exit(0)
		}()
	}

	log.Info("spot service starting", zap.String("addr", *addr))
	if err := cfg.Listen(r, *addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
