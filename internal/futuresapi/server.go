// Package futuresapi 是合约交易服务的「应用层 + HTTP Handler 层」。
//
// 分层定位：
//   - 下游领域依赖：internal/futures（强平/资金费/标记价引擎）、ledger（复式记账账本）、
//     settlement（链上充提网关模拟）、oracle（预言机指数价）、matching（撮合引擎）。
//   - 本包负责把上述领域组件「装配」成可运行服务（回调接线、后台循环、风控参数），
//     并通过 RegisterRoutes 暴露 HTTP 边界。不含 Store/Model（账本持久化由 ledger 负责）。
//   - cmd/futures/main.go 仅做进程级装配：读配置、建 ledger、MySQL 持久化
//     生命周期（恢复/种子/信号/保存），再调用 NewServer + RegisterRoutes + Run。
//
// 这样 cmd 保持薄装配层，业务逻辑与 HTTP 边界集中在 futuresapi，符合项目服务分层约定。
package futuresapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/client"
	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/oracle"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/risk"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/coldlar/crypto-exchange/internal/ws"
)

// ginH 是 gin.H 的等价别名（map[string]interface{}），用于构造广播消息体。
// hub.Broadcast 接收 interface{}，故无需直接依赖 gin 即可构造。
type ginH = map[string]interface{}

// Server 聚合合约交易服务运行所需的全部依赖与生命周期。
type Server struct {
	log       *zap.Logger
	ledgerSvc *ledger.Ledger
	cfg       *config.Config
	dsn       string

	hub           *ws.Hub
	symbols       []string
	oracleSvc     *oracle.Oracle
	funding       *futures.FundingManager
	markCalcs     map[string]*futures.MarkPriceCalculator
	matcher       matching.Matcher
	client        *client.Client
	liquidator    *futures.Liquidator
	feeModel      *settlement.FeeModel
	chainGateway  settlement.DepositGateway
	chainWithdraw settlement.WithdrawGateway
	// chainAuthorizer 是提现门控能力（M4）。网关实现 WithdrawAuthorizer 时持有之，用于在
	// SubmitWithdraw 前登记已通过风控 + ledger 预冻 hold 的提现授权；nil（网关未实现）时跳过。
	chainAuthorizer settlement.WithdrawAuthorizer

	// 风控：提现强制路径在冻结资金前调用 riskSvc.CheckWithdraw 网关。
	riskSvc    *risk.Service
	userSvcURL string
	kycFetcher func(c *gin.Context) (int, error) // 可注入，便于测试；默认从 user 服务取 kyc_level

	// 交易白名单（地址簿）：用户维护的可信提现/转账地址。
	addrBookMu sync.Mutex
	addrBook   map[int64][]AddrBookEntry
	addrSeq    int64

	// 内部划转：资金账户(可用) ⇄ 合约保证金(冻结)。与账本可用余额分离计账。
	marginMu   sync.Mutex
	marginAcct map[int64]map[string]float64 // uid -> asset -> 保证金余额

	// 持仓止盈止损（TP/SL）：按 (uid|symbol|side) 持久化。
	// tpsl     内存热缓存；tpslStore 写穿持久化层（MySQL 或 mem 降级），重启恢复。
	tpslMu    sync.Mutex
	tpsl      map[int64]map[string]TPState
	tpslStore TPSLStore

	// 通知服务（§37 业务事件→通知）：强平/保证金预警等业务事件写入站内信。
	// 内存存储，重启后已读状态丢失，不影响通知内容。
	notifSvc        *notification.Service
	marginWarned    map[string]bool // "uid:symbol" → 已预警（防止重复发送）
	marginWarnedMu  sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer 装配合约交易服务。
//
// 多实例收敛（见 DEVELOPMENT_TASKS §18）：撮合不再由本进程内的 matching.Engine 完成，
// 而是改为调用独立的 cmd/matching 服务（matcher 为 *client.Client，满足 matching.Matcher）。
// 成交/深度由 cmd/matching 经 WebSocket 推送，本服务的 onTrade 由该推送驱动（更新标记价、
// 触发强平、广播行情）；强平平仓则通过 matcher.MatchNow 同步向 cmd/matching 索取真实成交。
// 成交流由 cmd/matching 统一发布到 Kafka（exchange.trades），本服务不再重复发布。
// 这样全交易所的匹配权威唯一，订单簿不再随服务进程分裂。
//
// 调用方约定：在调用前必须已经完成账本快照恢复或种子充值（本函数会配置账本风控参数并启动巡检，
// 但不负责账本初始状态的载入）。onTrade 由 WS 推送在成交后调用，此时 s.liquidator 已赋值。
func NewServer(ledgerSvc *ledger.Ledger, log *zap.Logger, cfg *config.Config, dsn, matchingURL string, oracleConf oracle.OracleConf, chainRPC settlement.ChainRPCConfig, riskSvc *risk.Service, userSvcURL string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		log:       log,
		ledgerSvc: ledgerSvc,
		cfg:       cfg,
		dsn:       dsn,
		hub:       ws.NewHub(),
		symbols:   []string{"BTC_USDT_PERP", "ETH_USDT_PERP"},
		markCalcs: make(map[string]*futures.MarkPriceCalculator),
		ctx:       ctx,
		cancel:    cancel,

		riskSvc:    riskSvc,
		userSvcURL: userSvcURL,
		kycFetcher: newKYCFetcher(userSvcURL),

		addrBook:   make(map[int64][]AddrBookEntry),
		marginAcct: make(map[int64]map[string]float64),
		tpsl:       make(map[int64]map[string]TPState),
	}

	// 账本资金安全防线（演示值，生产按资产风险配置）。
	ledgerSvc.SetHotWalletCap("USDT", settlement.AssetAmountFromInt64(150000, settlement.AssetDecimalsByName("USDT")))
	ledgerSvc.SetHotWalletCap("ETH", settlement.AssetAmountFromInt64(50, settlement.AssetDecimalsByName("ETH")))
	ledgerSvc.SetHotWalletCap("BTC", settlement.AssetAmountFromInt64(5, settlement.AssetDecimalsByName("BTC")))
	ledgerSvc.SetWithdrawHoldPeriod(30 * time.Second)
	ledgerSvc.SetDailyWithdrawLimit("USDT", settlement.AssetAmountFromInt64(50000, settlement.AssetDecimalsByName("USDT")))
	ledgerSvc.SetDailyWithdrawLimit("ETH", settlement.AssetAmountFromInt64(10, settlement.AssetDecimalsByName("ETH")))
	ledgerSvc.SetDailyWithdrawLimit("BTC", settlement.AssetAmountFromInt64(1, settlement.AssetDecimalsByName("BTC")))
	ledgerSvc.SetAddressVerifyPeriod(10 * time.Second)
	ledgerSvc.EnableRiskEngine(true, true)
	ledgerSvc.SetRiskThresholds(60*time.Second, 30000, 5, 3)
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]settlement.AssetAmount) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)

	// TP-SL 持久化：MySQL 可用时落库；不可用时退化为纯内存（重启丢失）。
	if dsn != "" {
		if db, derr := sql.Open("mysql", dsn); derr == nil {
			if perr := db.Ping(); perr == nil {
				if store, sErr := NewMySQLTPSLStore(db); sErr == nil {
					s.tpslStore = store
					log.Info("tpsl store: mysql")
				} else {
					log.Warn("tpsl mysql migrate failed, fallback to mem", zap.Error(sErr))
					_ = db.Close()
				}
			} else {
				log.Warn("tpsl mysql ping failed, fallback to mem", zap.Error(perr))
				_ = db.Close()
			}
		} else {
			log.Warn("tpsl sql.Open failed, fallback to mem", zap.Error(derr))
		}
	}
	if s.tpslStore == nil {
		s.tpslStore = NewMemTPSLStore()
		log.Info("tpsl store: in-memory")
	}
	// 启动时从持久层全量加载到内存热缓存。
	if loaded, lErr := s.tpslStore.LoadAll(); lErr == nil {
		s.tpsl = loaded
		log.Info("tpsl loaded from store", zap.Int("entries", tpslCount(s.tpsl)))
	} else {
		log.Warn("tpsl load failed, starting empty", zap.Error(lErr))
	}

	// §37 业务事件→通知：初始化通知服务（MySQL 持久化；不可用时退化为内存）。
	if dsn != "" {
		if ndb, nerr := sql.Open("mysql", dsn); nerr == nil {
			if nping := ndb.Ping(); nping == nil {
				if nstore, nsErr := notification.NewMySQLStore(ndb); nsErr == nil {
					s.notifSvc = notification.New(nstore)
					log.Info("notification store: mysql")
				} else {
					log.Warn("notification mysql migrate failed, fallback to mem", zap.Error(nsErr))
					_ = ndb.Close()
				}
			} else {
				log.Warn("notification mysql ping failed, fallback to mem", zap.Error(nping))
				_ = ndb.Close()
			}
		} else {
			log.Warn("notification sql.Open failed, fallback to mem", zap.Error(nerr))
		}
	}
	if s.notifSvc == nil {
		s.notifSvc = notification.New(notification.NewMemStore())
		log.Info("notification store: in-memory")
	}
	s.marginWarned = make(map[string]bool)

	// 指数价预言机：优先使用配置中的真实 REST 喂价（oracle.NewFromConfig）；
	// 未配置喂价源时回退到内置 StaticFeed 演示（模拟多交易所价差）。
	var oracleSvc *oracle.Oracle
	if len(oracleConf.Feeds) > 0 {
		oracleSvc = oracle.NewFromConfig(oracleConf)
	} else {
		baseIndex := map[string]float64{
			"BTC_USDT_PERP": 50000,
			"ETH_USDT_PERP": 3000,
		}
		oracleFeeds := make(map[string][]oracle.PriceFeed)
		for sym, base := range baseIndex {
			oracleFeeds[sym] = []oracle.PriceFeed{
				oracle.NewStaticFeed("binance", base),
				oracle.NewStaticFeed("okx", base*1.0008),
				oracle.NewStaticFeed("coinbase", base*0.9992),
			}
		}
		oracleSvc = oracle.New(oracle.Config{
			PollInterval: 3 * time.Second,
			Feeds:        oracleFeeds,
		})
	}
	s.oracleSvc = oracleSvc
	s.oracleSvc.Start()

	// 资金费率管理器（演示结算周期 30s）。
	s.funding = futures.NewFundingManager(30 * time.Second)
	for _, sym := range s.symbols {
		s.funding.Register(sym)
		if idx, ok := s.oracleSvc.IndexPrice(sym); ok {
			s.funding.UpdateIndexPrice(sym, idx)
		}
	}

	// 标记价格计算器：指数价 + 溢价 EMA。
	for _, sym := range s.symbols {
		mc := futures.NewMarkPriceCalculator(0)
		if idx, ok := s.oracleSvc.IndexPrice(sym); ok {
			mc.SetIndex(idx)
		}
		s.markCalcs[sym] = mc
	}

	// 撮合客户端：收敛到 cmd/matching 服务（匹配权威唯一，支持多实例单写者部署）。
	// 成交/深度经 WebSocket 推送，本服务据此驱动 onTrade 与前端行情广播。
	s.client = client.New(matchingURL)
	s.matcher = s.client
	go func() {
		if err := s.client.Watch(ctx, s.symbols,
			func(ev client.TradeEvent) { s.onTrade(ev.Symbol, ev.Trade) },
			func(ev client.DepthEvent) {
				s.hub.Broadcast(ev.Symbol, gin.H{
					"type": "depth",
					"data": gin.H{"bids": ev.Bids, "asks": ev.Asks},
				})
			}); err != nil && ctx.Err() == nil {
			s.log.Warn("futures matching watch exited", zap.Error(err))
		}
	}()

	// 强平器：各类保证金/保险基金回调。
	s.liquidator = futures.NewLiquidator(s.onLiquidation)
	s.liquidator.SetPartialRatio(0.5)
	s.liquidator.SetInsuranceProvider(func() float64 {
		bal, _, _ := ledgerSvc.Balance(ledger.SysInsurance, "USDT")
		return bal.HumanFloat()
	})
	s.liquidator.SetADLCallback(func(ev futures.ADLEvent) {
		ref := fmt.Sprintf("adl:%d:%s", ev.UserID, ev.Symbol)
		// M5/F5：引擎派生金额为 float，落账前拦截 NaN/Inf（避免静默归零记坏账），并对正常浮点
		// 尾差四舍五入（AssetAmountFromFloatSafe）。非法值跳过本次结算，避免污染账本。
		amt, err := settlement.AssetAmountFromFloatSafe(ev.ProfitCovered, settlement.AssetDecimalsByName("USDT"))
		if err != nil {
			log.Error("adl settle skipped: invalid float", zap.Int64("user", ev.UserID), zap.Error(err))
			return
		}
		// F3 原子：ADL 扣减用户保证金 + 转入保险基金，整体经 Batch 执行，任一失败整组回滚
		// （避免"用户已扣、保险未收"的账本失衡）。Debit/Credit 不按 ref 去重，ref 仅作流水标签。
		ops := []ledger.Op{
			{Kind: ledger.OpDebit, User: ev.UserID, Asset: "USDT", Amount: amt, Biz: "adl", Ref: ref},
			{Kind: ledger.OpCredit, User: ledger.SysInsurance, Asset: "USDT", Amount: amt, Biz: "adl", Ref: ref},
		}
		if err := ledgerSvc.Batch(ops); err != nil {
			log.Error("adl settle failed", zap.Int64("user", ev.UserID), zap.Error(err))
		}
		s.hub.Broadcast(ev.Symbol, ginH{"type": "adl", "data": ev})
		log.Warn("auto-deleveraging",
			zap.Int64("user", ev.UserID), zap.String("side", sideName(ev.Side)),
			zap.Float64("reduced", ev.ReducedSize), zap.Float64("profit_covered", ev.ProfitCovered))
	})
	s.liquidator.SetDeficitPayer(func(deficit float64) {
		ref := fmt.Sprintf("deficit:%d", time.Now().UnixNano())
		amt, err := settlement.AssetAmountFromFloatSafe(deficit, settlement.AssetDecimalsByName("USDT"))
		if err != nil {
			log.Error("liquidation deficit settle skipped: invalid float", zap.Error(err))
			return
		}
		// F3 原子：保险基金垫付穿仓亏损 → 记为清算损失，整体 Batch（消除原 _= 吞错导致的失衡）。
		ops := []ledger.Op{
			{Kind: ledger.OpDebit, User: ledger.SysInsurance, Asset: "USDT", Amount: amt, Biz: "liquidation_deficit", Ref: ref},
			{Kind: ledger.OpCredit, User: ledger.SysLiquidationLoss, Asset: "USDT", Amount: amt, Biz: "liquidation_deficit", Ref: ref},
		}
		if err := ledgerSvc.Batch(ops); err != nil {
			log.Error("liquidation deficit settle failed", zap.Error(err))
		}
		log.Warn("liquidation deficit paid by insurance", zap.Float64("deficit", deficit))
	})
	s.liquidator.SetSocializeCallback(func(shares []futures.SocializedLossEvent) {
		for _, sh := range shares {
			ref := fmt.Sprintf("socialize:%d:%s", sh.UserID, sh.Symbol)
			amt, err := settlement.AssetAmountFromFloatSafe(sh.Share, settlement.AssetDecimalsByName("USDT"))
			if err != nil {
				log.Error("socialize settle skipped: invalid float", zap.Int64("user", sh.UserID), zap.Error(err))
				continue
			}
			// F3 原子：社会化分摊从用户扣减 → 转入保险基金，整体 Batch。
			ops := []ledger.Op{
				{Kind: ledger.OpDebit, User: sh.UserID, Asset: "USDT", Amount: amt, Biz: "socialized_loss", Ref: ref},
				{Kind: ledger.OpCredit, User: ledger.SysInsurance, Asset: "USDT", Amount: amt, Biz: "socialized_loss", Ref: ref},
			}
			if err := ledgerSvc.Batch(ops); err != nil {
				log.Error("socialize settle failed", zap.Int64("user", sh.UserID), zap.Error(err))
			}
		}
		s.hub.Broadcast(shares[0].Symbol, ginH{"type": "socialized", "data": shares})
		log.Warn("socialized loss applied", zap.Int("accounts", len(shares)))
	})
	for _, sym := range s.symbols {
		s.liquidator.Register(sym)
	}
	// 把强平单接入撮合引擎：强平不再按标记价直接记账减仓，而是作为市价单送入撮合引擎
	// 真实成交，据成交均价回填持仓；订单簿无流动性时由保险基金兜底成交（保证强平必定完成）。
	s.liquidator.SetLiquidationCloser(s.liquidationCloser)

	// 多链/多资产手续费模型。
	s.feeModel = settlement.NewFeeModel()
	s.feeModel.Register(settlement.ChainETH, "USDT", 0.1, 0)
	s.feeModel.Register(settlement.ChainETH, "ETH", 0.001, 0)
	s.feeModel.Register(settlement.ChainBTC, "BTC", 0.0005, 0)
	s.feeModel.Register(settlement.ChainTRON, "USDT", 1, 0)
	s.feeModel.Register(settlement.ChainLTC, "LTC", 0.001, 0) // 原生 LTC（8 位小数，对标 BTC 尘额）
	s.feeModel.Register(settlement.ChainDOGE, "DOGE", 1.0, 0) // 原生 DOGE（8 位小数）

	// 链上充提网关及其事件监听。充值/提现网关均按配置在「真实 RPC」与「模拟」间切换
	// （T-03 链上 RPC 半边脚手架，fail-degraded：未配置回退模拟）。
	s.chainGateway = settlement.NewDepositGateway(chainRPC)
	s.chainGateway.Start()
	s.chainGateway.StartScan(ctx)
	s.chainWithdraw = settlement.NewWithdrawGateway(chainRPC)
	// M4：若提现网关实现门控能力，则持有之，以便在广播前登记授权（完整网关侧鉴权门控）。
	if auth, ok := s.chainWithdraw.(settlement.WithdrawAuthorizer); ok {
		s.chainAuthorizer = auth
	}
	// M4：若网关为真实 RPC 网关（实现 SetWithdrawHoldResolver），注入 ledger hold 解析器，
	// 使离线签名器在广播前真正校验 hold 存在/状态/要素一致（绑定真实提现记录，纵深防御）。
	if rgw, ok := s.chainWithdraw.(*settlement.RPCWithdrawGateway); ok {
		rgw.SetWithdrawHoldResolver(ledgerHoldResolver{ledgerSvc})
	}
	s.chainWithdraw.Start()
	s.startChainWatchers()

	// 资金费率结算后台循环。
	go s.fundingLoop()
	// 独立强平扫描循环（与成交解耦，保证无成交时价格击穿也能强平）。
	go s.liqScanLoop()

	return s
}

// startChainWatchers 启动链上充值/提现的确认、回滚事件监听 goroutine。
// 这些 goroutine 随 Server.ctx 取消而退出（Watch 返回的 channel 不再由网关关闭，
// 消费者经 ctx.Done() 退出，避免向已关闭 channel 发送触发 panic）。
func (s *Server) startChainWatchers() {
	// 充值确认入账。
	go func() {
		ch, err := s.chainGateway.Watch(s.ctx)
		if err != nil {
			s.log.Error("chain gateway watch failed", zap.Error(err))
			return
		}
		for {
			select {
			case ev := <-ch:
				if err := s.ledgerSvc.ReceiveOnChain(ev.UserID, ev.Asset, ev.Amount, ev.TxHash); err != nil {
					s.log.Error("on-chain credit failed", zap.String("tx", ev.TxHash), zap.Error(err))
					continue
				}
				s.log.Info("on-chain deposit credited",
					zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
					zap.Float64("amount", ev.Amount.HumanFloat()), zap.String("tx", ev.TxHash))
				s.hub.Broadcast("SYS", ginH{"type": "chain_deposit", "data": ev})
				s.publishDepositNotice(ev)
			case <-s.ctx.Done():
				return
			}
		}
	}()

	// 充值孤块/重组回滚。
	go func() {
		rch, err := s.chainGateway.WatchRollback(s.ctx)
		if err != nil {
			s.log.Error("chain gateway watch rollback failed", zap.Error(err))
			return
		}
		for {
			select {
			case ev := <-rch:
				badDebt, err := s.ledgerSvc.ReverseOnChain(ev.UserID, ev.Asset, ev.Amount, ev.TxHash)
				if err != nil {
					s.log.Error("on-chain rollback failed", zap.String("tx", ev.TxHash), zap.Error(err))
					continue
				}
				if badDebt.Sign() > 0 {
					s.log.Error("on-chain deposit reverted with BAD DEBT (user already spent funds)",
						zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
						zap.Float64("amount", ev.Amount.HumanFloat()), zap.Float64("bad_debt", badDebt.HumanFloat()),
						zap.String("tx", ev.TxHash))
				} else {
					s.log.Warn("on-chain deposit reverted (orphan block)",
						zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
						zap.Float64("amount", ev.Amount.HumanFloat()), zap.String("tx", ev.TxHash))
				}
				s.hub.Broadcast("SYS", ginH{"type": "chain_rollback", "data": ev, "bad_debt": badDebt})
			case <-s.ctx.Done():
				return
			}
		}
	}()

	// 提现确认划出 / 失败回退。
	go func() {
		wch, err := s.chainWithdraw.WatchWithdraw(s.ctx)
		if err != nil {
			s.log.Error("chain withdraw watch failed", zap.Error(err))
			return
		}
		for {
			select {
			case ev := <-wch:
				total := ev.Amount.Add(ev.Fee)
				switch ev.Status {
				case settlement.WithdrawCredited:
					if err := s.ledgerSvc.SettleWithdraw(ev.UserID, ev.Asset, ev.Amount, ev.Fee, ev.TxHash); err != nil {
						s.log.Error("withdraw settle failed", zap.String("tx", ev.TxHash), zap.Error(err))
						continue
					}
				s.log.Info("on-chain withdraw settled",
					zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
					zap.Float64("amount", ev.Amount.HumanFloat()), zap.Float64("fee", ev.Fee.HumanFloat()),
					zap.String("tx", ev.TxHash))
				s.hub.Broadcast("SYS", ginH{"type": "chain_withdraw", "data": ev})
				s.publishWithdrawNotice(ev)
				case settlement.WithdrawFailed:
					if err := s.ledgerSvc.UnfreezeWithdraw(ev.UserID, ev.Asset, total); err != nil {
						s.log.Error("withdraw rollback failed", zap.String("tx", ev.TxHash), zap.Error(err))
						continue
					}
					s.log.Warn("on-chain withdraw failed, rolled back",
						zap.Int64("user", ev.UserID), zap.String("tx", ev.TxHash))
					s.hub.Broadcast("SYS", ginH{"type": "chain_withdraw_failed", "data": ev})
				}
			case <-s.ctx.Done():
				return
			}
		}
	}()

	// 提现孤块/重组回滚。
	go func() {
		wrh, err := s.chainWithdraw.WatchWithdrawRollback(s.ctx)
		if err != nil {
			s.log.Error("chain withdraw watch rollback failed", zap.Error(err))
			return
		}
		for {
			select {
			case ev := <-wrh:
				total := ev.Amount.Add(ev.Fee)
				if err := s.ledgerSvc.ReverseWithdraw(ev.UserID, ev.Asset, ev.Amount, ev.Fee, ev.TxHash); err != nil {
					s.log.Error("withdraw rollback failed", zap.String("tx", ev.TxHash), zap.Error(err))
					continue
				}
				if err := s.ledgerSvc.UnfreezeWithdraw(ev.UserID, ev.Asset, total); err != nil {
					s.log.Error("withdraw rollback unfreeze failed", zap.String("tx", ev.TxHash), zap.Error(err))
					continue
				}
				s.log.Warn("on-chain withdraw reverted (orphan block), funds returned",
					zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
					zap.Float64("amount", ev.Amount.HumanFloat()), zap.Float64("fee", ev.Fee.HumanFloat()),
					zap.String("tx", ev.TxHash))
				s.hub.Broadcast("SYS", ginH{"type": "chain_withdraw_rollback", "data": ev})
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

// onTrade 是撮合引擎的成交流回调：更新标记价、触发强平、广播行情、发布成交流到消息总线。
func (s *Server) onTrade(symbol string, t matching.Trade) {
	mc := s.markCalcs[symbol]
	if idx, ok := s.oracleSvc.IndexPrice(symbol); ok {
		mc.SetIndex(idx)
		s.funding.UpdateIndexPrice(symbol, idx)
	}
	mc.UpdateContractPrice(t.Price.Float()) // 标记价引擎为浮点接口（内部 roundSatoshi 量化），边界转换
	mark := mc.MarkPrice()

	if liqEvents := s.liquidator.UpdateMarkPrice(symbol, mark); len(liqEvents) > 0 {
		s.broadcastLiquidations(liqEvents)
	}
	s.hub.Broadcast(symbol, ginH{
		"type": "trade",
		"data": t,
		"mark": mark,
	})
}

// onLiquidation 是强平器的强平事件回调：处理部分强平释放保证金与整仓强平没收保证金入保险基金。
func (s *Server) onLiquidation(ev futures.LiquidationEvent) {
	ref := fmt.Sprintf("liq:%d:%s", ev.UserID, ev.Symbol)
	// M5/F5：强平金额由引擎 float 派生，落账前拦截 NaN/Inf 并四舍五入尾差；非法值跳过整笔结算。
	marginAmt, err := settlement.AssetAmountFromFloatSafe(ev.Margin, settlement.AssetDecimalsByName("USDT"))
	if err != nil {
		s.log.Error("liquidation settle skipped: invalid margin float", zap.Int64("user", ev.UserID), zap.Error(err))
		return
	}
	realizedAmt, err := settlement.AssetAmountFromFloatSafe(ev.Realized, settlement.AssetDecimalsByName("USDT"))
	if err != nil {
		s.log.Error("liquidation settle skipped: invalid realized float", zap.Int64("user", ev.UserID), zap.Error(err))
		return
	}
	if ev.Partial {
		if ev.Mode == futures.Cross {
			if ev.Realized >= 0 {
				_ = s.ledgerSvc.Freeze(ev.UserID, "USDT", realizedAmt)
			} else {
				_ = s.ledgerSvc.Unfreeze(ev.UserID, "USDT", realizedAmt.Neg())
			}
		} else {
			// F3 原子：释放冻结保证金 + 实现盈亏（贷记/借记用户），整体 Batch。
			ops := []ledger.Op{{Kind: ledger.OpUnfreeze, User: ev.UserID, Asset: "USDT", Amount: marginAmt}}
			if ev.Realized >= 0 {
				ops = append(ops, ledger.Op{Kind: ledger.OpCredit, User: ev.UserID, Asset: "USDT", Amount: realizedAmt, Biz: "partial_liq", Ref: ref})
			} else {
				ops = append(ops, ledger.Op{Kind: ledger.OpDebit, User: ev.UserID, Asset: "USDT", Amount: realizedAmt.Neg(), Biz: "partial_liq", Ref: ref})
			}
			if err := s.ledgerSvc.Batch(ops); err != nil {
				s.log.Error("partial liquidation settle failed", zap.Int64("user", ev.UserID), zap.Error(err))
			}
		}
		s.log.Info("partial liquidation",
			zap.Int64("user", ev.UserID), zap.String("mode", modeName(ev.Mode)),
			zap.Float64("closed", ev.Size), zap.Float64("remaining", ev.RemainingSize),
			zap.Float64("realized", ev.Realized))
		return
	}

	// F3 原子：释放冻结保证金 + 没收入保险基金，整体 Batch（任一失败整组回滚，避免"释放却未没收"的失衡）。
	ops := []ledger.Op{
		{Kind: ledger.OpUnfreeze, User: ev.UserID, Asset: "USDT", Amount: marginAmt},
		{Kind: ledger.OpDebit, User: ev.UserID, Asset: "USDT", Amount: marginAmt, Biz: "liquidation", Ref: ref},
		{Kind: ledger.OpCredit, User: ledger.SysInsurance, Asset: "USDT", Amount: marginAmt, Biz: "liquidation", Ref: ref},
	}
	if err := s.ledgerSvc.Batch(ops); err != nil {
		s.log.Error("liquidation settle failed", zap.Int64("user", ev.UserID), zap.Error(err))
	}
	s.log.Info("liquidation settled",
		zap.Int64("user", ev.UserID),
		zap.Float64("margin_forfeited", ev.Margin),
		zap.Float64("closed", ev.Size),
		zap.Float64("realized", ev.Realized))
}

// liquidationCloser 强平平仓执行器：把强平单作为市价单同步送入撮合引擎成交，
// 返回真实成交（含成交均价）。流程：
//  1. 市价单（Price=0）经 engine.MatchNow 与订单簿真实流动性撮合；
//  2. 若订单簿流动性不足（未完全成交），剩余部分由保险基金（SysLiquidationLoss）
//     作为兜底对手方在标记价成交，确保强平必定完成（与真实交易所保险基金兜底一致）；
//  3. 返回加权成交均价，供强平引擎据真实成交价回填持仓、计算实现盈亏与穿仓亏损。
//
// 注意：MatchNow 同步且不触发 onTrade，可安全在强平扫描上下文（onTrade/liqScanLoop）调用，
// 不会因 onTrade 再次触发 UpdateMarkPrice 造成重入。
func (s *Server) liquidationCloser(symbol string, userID int64, side futures.PosSide, qty, mark float64) futures.LiquidationFill {
	ms := matching.Sell // 平多=卖（吃买方流动性）；平空=买（吃卖方流动性）
	if side == futures.Short {
		ms = matching.Buy
	}
	// 边界定点化：qty/mark 来自强平引擎（float），此处按交易对配置的 scale 对齐为 Fixed，
	// 与送入撮合的订单一致；成交汇总（filled/notional）全程定点整数运算，消除累计漂移。
	qtyF := matching.FixedFromFloat(qty, s.cfg.QtyScale(symbol))
	markF := matching.FixedFromFloat(mark, s.cfg.PriceScale(symbol))
	o := &matching.Order{
		UserID: userID, // taker 标识为被强平用户（其持仓正被平仓）
		Side:   ms,
		Price:  matching.Fixed{}, // 市价单
		Qty:    qtyF,
		Time:   time.Now().UnixNano(),
		Market: "futures",
	}
	trades, fully := s.matcher.MatchNow(symbol, o, false)
	var filled, notional matching.Fixed
	for _, t := range trades {
		filled = filled.Add(t.Qty)
		notional = notional.Add(t.Price.Mul(t.Qty))
	}
	if !fully {
		rem := qtyF.Sub(filled)
		if rem.IsPositive() {
			// 保险基金兜底成交：maker 为 SysLiquidationLoss（破产价/标记价），保证强平必定完成。
			trades = append(trades, matching.Trade{
				Price:     markF,
				Qty:       rem,
				TakerID:   userID,
				MakerID:   ledger.SysLiquidationLoss,
				TakerSide: ms,
			})
			filled = qtyF
			notional = notional.Add(markF.Mul(rem))
		}
	}
	avg := mark
	if filled.IsPositive() {
		avg = notional.Div(filled).Float()
	}
	return futures.LiquidationFill{Filled: filled.Float(), AvgPrice: avg, Trades: len(trades)}
}

// liqScanInterval 独立强平扫描周期。
// 与成交驱动的 onTrade 并行：即便长时间无成交，只要预言机指数价变动击穿强平价，
// 也能被本循环扫描并强平（修复原实现「强平仅在撮合成交时触发」的缺口）。
// 2s 为演示值；生产可按风控要求收紧到秒级甚至亚秒级。
const liqScanInterval = 2 * time.Second

// liqScanLoop 周期性用预言机指数价刷新标记价，并触发强平扫描。
// 不依赖撮合成交：mark = 指数价 + 溢价EMA（无成交时溢价EMA=0，mark=指数价），
// 因此指数价击穿强平价即可触发，与是否有成交无关。UpdateMarkPrice 对已强平
// 持仓幂等（二次扫描不再产生事件），可安全高频调用。
func (s *Server) liqScanLoop() {
	ticker := time.NewTicker(liqScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			for _, sym := range s.symbols {
				if _, ok := s.liquidator.Book(sym); !ok {
					continue
				}
				if idx, ok := s.oracleSvc.IndexPrice(sym); ok {
					s.markCalcs[sym].SetIndex(idx)
				}
				mark := s.markCalcs[sym].MarkPrice()
				if evs := s.liquidator.UpdateMarkPrice(sym, mark); len(evs) > 0 {
					s.broadcastLiquidations(evs)
				}
				// §37 保证金预警：扫描所有仓位，标记价接近强平价时发送预警通知。
				s.emitMarginWarnings(sym, mark)
			}
		}
	}
}

// MarginWarnRatio 保证金预警阈值：保证金率低于此值时触发预警通知（如 1.2 = 保证金率 120%）。
// 略高于 SafeMarginRatio(1.1)，为用户留出反应时间。
const MarginWarnRatio = 1.2

// emitMarginWarnings 扫描所有仓位，对保证金率低于预警阈值的仓位发送站内通知。
// 每个 (user, symbol) 组合仅预警一次（内存去重），防止行情震荡期重复打扰。
func (s *Server) emitMarginWarnings(symbol string, mark float64) {
	if s.notifSvc == nil || mark <= 0 {
		return
	}
	positions := s.liquidator.AllPositions(symbol)
	for _, p := range positions {
		if p.Size <= 0 {
			continue
		}
		var marginRatio float64
		if p.Mode == futures.Isolated {
			// 逐仓：marginRatio = (margin + UPNL) / notional
			upnl := p.UPNL(mark)
			notional := p.Notional(mark)
			if notional > 0 {
				marginRatio = (p.Margin + upnl) / notional
			}
		} else {
			// 全仓：用强平价近似推导保证金率
			// mark 接近强平价 → 保证金率低
			if p.LiqPriceVal > 0 && mark > 0 {
				if p.Side == futures.Long {
					marginRatio = mark / p.LiqPriceVal
				} else {
					marginRatio = p.LiqPriceVal / mark
				}
			}
		}
		if marginRatio <= 0 || marginRatio >= MarginWarnRatio {
			continue
		}
		key := fmt.Sprintf("%d:%s", p.UserID, symbol)
		s.marginWarnedMu.Lock()
		if s.marginWarned[key] {
			s.marginWarnedMu.Unlock()
			continue
		}
		s.marginWarned[key] = true
		s.marginWarnedMu.Unlock()

		sideStr := sideName(p.Side)
		s.notifSvc.Publish(notification.PublishInput{
			UserID: p.UserID,
			Type:   notification.TypeMarginWarning,
			Title:  "保证金不足预警",
			Body: fmt.Sprintf("您的 %s %s 仓位保证金率 %.1f%%，接近强平价 %.2f，请及时追加保证金",
				symbol, sideStr, marginRatio*100, p.LiqPriceVal),
		})
	}
}

// broadcastLiquidations 广播强平事件（onTrade 与 liqScanLoop 共用）：记录审计日志、推送 WS 并发送站内通知。
func (s *Server) broadcastLiquidations(evs []futures.LiquidationEvent) {
	for _, ev := range evs {
		s.log.Warn("liquidation",
			zap.Int64("user", ev.UserID),
			zap.String("side", sideName(ev.Side)),
			zap.Float64("liq_price", ev.LiqPrice),
			zap.Float64("fee", ev.Fee),
			zap.Bool("partial", ev.Partial))
		s.hub.Broadcast(ev.Symbol, ginH{"type": "liquidation", "data": ev})
		// §37 强平站内通知：写入用户通知中心。
		s.publishLiquidationNotice(ev)
	}
}

// publishLiquidationNotice 向被强平用户发送站内通知（§37 业务事件→通知）。
// 部分强平与全额强平分别标题，body 含标的/方向/强平价/手续费等关键风控信息。
func (s *Server) publishLiquidationNotice(ev futures.LiquidationEvent) {
	if s.notifSvc == nil {
		return
	}
	title := "合约仓位被强平"
	body := fmt.Sprintf("您的 %s %s 仓位已被强制平仓", ev.Symbol, sideName(ev.Side))
	if ev.Partial {
		title = "合约仓位部分强平"
		body = fmt.Sprintf("您的 %s %s 仓位已被部分强平（平仓 %.4f，剩余 %.4f）",
			ev.Symbol, sideName(ev.Side), ev.Size, ev.RemainingSize)
	}
	body += fmt.Sprintf("。标记价 %.2f，强平手续费 %.4f USDT", ev.LiqPrice, ev.Fee)
	if ev.Realized != 0 {
		body += fmt.Sprintf("，实现盈亏 %.2f USDT", ev.Realized)
	}
	s.notifSvc.Publish(notification.PublishInput{
		UserID: ev.UserID,
		Type:   notification.TypeLiquidation,
		Title:  title,
		Body:   body,
	})
}

// publishDepositNotice 充值到账通知。
func (s *Server) publishDepositNotice(ev settlement.DepositEvent) {
	if s.notifSvc == nil {
		return
	}
	s.notifSvc.Publish(notification.PublishInput{
		UserID: ev.UserID,
		Type:   notification.TypeDepositArrived,
		Title:  "充值到账",
		Body:   fmt.Sprintf("您的 %s 充值 %.4f 已到账，交易哈希 %s", ev.Asset, ev.Amount.HumanFloat(), ev.TxHash),
	})
}

// publishWithdrawNotice 提现完成通知。
func (s *Server) publishWithdrawNotice(ev settlement.WithdrawEvent) {
	if s.notifSvc == nil {
		return
	}
	s.notifSvc.Publish(notification.PublishInput{
		UserID: ev.UserID,
		Type:   notification.TypeWithdrawDone,
		Title:  "提现已完成",
		Body:   fmt.Sprintf("您的 %s 提现 %.4f 已到账链上，交易哈希 %s", ev.Asset, ev.Amount.HumanFloat(), ev.TxHash),
	})
}

// fundingLoop 周期性对所有持仓结算一次资金费用（标记价 + 溢价 EMA + 持仓，净额零和转账）。
func (s *Server) fundingLoop() {
	ticker := time.NewTicker(s.funding.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			for _, sym := range s.symbols {
				if _, ok := s.liquidator.Book(sym); !ok {
					continue
				}
				if idx, ok := s.oracleSvc.IndexPrice(sym); ok {
					s.markCalcs[sym].SetIndex(idx)
					s.funding.UpdateIndexPrice(sym, idx)
				}
				ev := s.funding.Settle(sym, s.markCalcs[sym].MarkPrice(), s.markCalcs[sym].PremiumEMA(), s.liquidator.AllPositions(sym))
				if len(ev.Payments) == 0 {
					continue
				}
				ref := fmt.Sprintf("funding:%s:%d", sym, ev.Time)
				var totalPaid float64
				for _, p := range ev.Payments {
					totalPaid += p.Payment
					cross := s.liquidator.ModeOf(sym, p.UserID) == futures.Cross
					switch {
					case p.Payment < 0:
						pay := -p.Payment
						// M5/F5：资金费由引擎 float 派生，落账前拦截 NaN/Inf 并四舍五入尾差；非法值跳过该笔。
						payAmt, ferr := settlement.AssetAmountFromFloatSafe(pay, settlement.AssetDecimalsByName("USDT"))
						if ferr != nil {
							s.log.Error("funding debit skipped: invalid float", zap.Int64("user", p.UserID), zap.Error(ferr))
							continue
						}
						// F3 原子：跨仓先解冻保证金，再转账给资金费池；整体 Batch，失败整组回滚。
						ops := []ledger.Op{
							{Kind: ledger.OpTransfer, From: p.UserID, To: ledger.SysFundingPool, Asset: "USDT", Amount: payAmt, Biz: "funding", Ref: ref},
						}
						if cross {
							ops = append([]ledger.Op{{Kind: ledger.OpUnfreeze, User: p.UserID, Asset: "USDT", Amount: payAmt}}, ops...)
						}
						if err := s.ledgerSvc.Batch(ops); err != nil {
							s.log.Error("funding debit failed", zap.Int64("user", p.UserID), zap.Error(err))
							continue
						}
						if cross {
							s.liquidator.AdjustCrossBalance(sym, p.UserID, -pay)
						}
					case p.Payment > 0:
						// M5/F5：资金费由引擎 float 派生，落账前拦截 NaN/Inf 并四舍五入尾差；非法值跳过该笔。
						payAmt, ferr := settlement.AssetAmountFromFloatSafe(p.Payment, settlement.AssetDecimalsByName("USDT"))
						if ferr != nil {
							s.log.Error("funding credit skipped: invalid float", zap.Int64("user", p.UserID), zap.Error(ferr))
							continue
						}
						// F3 原子：资金费池转账给用户，跨仓再冻结保证金；整体 Batch，失败整组回滚。
						ops := []ledger.Op{
							{Kind: ledger.OpTransfer, From: ledger.SysFundingPool, To: p.UserID, Asset: "USDT", Amount: payAmt, Biz: "funding", Ref: ref},
						}
						if cross {
							ops = append(ops, ledger.Op{Kind: ledger.OpFreeze, User: p.UserID, Asset: "USDT", Amount: payAmt})
						}
						if err := s.ledgerSvc.Batch(ops); err != nil {
							s.log.Error("funding credit failed", zap.Int64("user", p.UserID), zap.Error(err))
							continue
						}
						if cross {
							s.liquidator.AdjustCrossBalance(sym, p.UserID, p.Payment)
						}
					}
				}
				s.log.Info("funding settled",
					zap.String("symbol", sym),
					zap.Float64("rate", ev.FundingRate),
					zap.Int("positions", len(ev.Payments)),
					zap.Float64("net", totalPaid))
				s.hub.Broadcast(sym, ginH{"type": "funding", "data": ev})
			}
		}
	}
}

// newKYCFetcher 返回从 user 服务取 kyc_level 的函数（注入到 Server，便于测试替换）。
//   - userSvcURL 为空：KYC 子项视为最高级、恒过（限额/黑名单仍生效）；生产必须配置 user 服务。
//   - 否则：转发调用方 Bearer Token，GET {base}/api/v1/user/me，解析 kyc_level；
//     非 200 / 网络错误 / 解码失败均返回 error，由调用方 fail-closed 拒绝提现（资金安全优先）。
func newKYCFetcher(userSvcURL string) func(c *gin.Context) (int, error) {
	if userSvcURL == "" {
		return func(c *gin.Context) (int, error) { return math.MaxInt, nil }
	}
	base := strings.TrimRight(userSvcURL, "/")
	client := &http.Client{Timeout: 3 * time.Second}
	return func(c *gin.Context) (int, error) {
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, base+"/api/v1/user/me", nil)
		if err != nil {
			return 0, err
		}
		if auth := c.GetHeader("Authorization"); auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("user service returned status %d", resp.StatusCode)
		}
		var body struct {
			KYCLevel int `json:"kyc_level"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return 0, err
		}
		return body.KYCLevel, nil
	}
}

// Close 停止所有后台组件（预言机、链上网关、对账巡检、资金费循环、行情订阅），并释放上下文。
func (s *Server) Close() {
	s.cancel()
	if s.oracleSvc != nil {
		s.oracleSvc.Stop()
	}
	if s.chainGateway != nil {
		s.chainGateway.Stop()
	}
	if s.chainWithdraw != nil {
		s.chainWithdraw.Stop()
	}
	s.ledgerSvc.StopReconciler()
}

// ledgerHoldResolver 将 ledger.WithdrawHold 适配为 settlement.WithdrawHoldResolver（M4 来源校验）：
// 提现广播前据此真正查询 ledger 提现冻结记录，校验 hold 存在/状态/要素一致，使离线签名器绑定
// 真实提现记录而非接受自证布尔。返回的去耦视图不含私钥/余额等敏感上下文。
type ledgerHoldResolver struct {
	l *ledger.Ledger
}

func (r ledgerHoldResolver) ResolveWithdrawHold(ctx context.Context, id string) (settlement.WithdrawHoldView, bool) {
	e, ok := r.l.WithdrawHold(id)
	if !ok {
		return settlement.WithdrawHoldView{}, false
	}
	return settlement.WithdrawHoldView{
		UserID:    e.UserID,
		Asset:     e.Asset,
		Chain:     settlement.Chain(e.Chain),
		Amount:    e.Amount,
		Fee:       e.Fee,
		Address:   e.Address,
		Finalized: e.Finalized,
		Cancelled: e.Cancelled,
	}, true
}

func tpslCount(m map[int64]map[string]TPState) int {
	c := 0
	for _, km := range m {
		c += len(km)
	}
	return c
}
