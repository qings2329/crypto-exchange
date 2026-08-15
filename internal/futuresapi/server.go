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
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/client"
	"github.com/coldlar/crypto-exchange/internal/oracle"
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
	dsn       string

	hub          *ws.Hub
	symbols      []string
	oracleSvc    *oracle.Oracle
	funding      *futures.FundingManager
	markCalcs    map[string]*futures.MarkPriceCalculator
	matcher      matching.Matcher
	client       *client.Client
	liquidator   *futures.Liquidator
	feeModel     *settlement.FeeModel
	chainGateway *settlement.MockChainGateway
	chainWithdraw *settlement.MockWithdrawGateway

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
func NewServer(ledgerSvc *ledger.Ledger, log *zap.Logger, dsn, matchingURL string, oracleConf oracle.OracleConf) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		log:       log,
		ledgerSvc: ledgerSvc,
		dsn:       dsn,
		hub:       ws.NewHub(),
		symbols:   []string{"BTC_USDT_PERP", "ETH_USDT_PERP"},
		markCalcs: make(map[string]*futures.MarkPriceCalculator),
		ctx:       ctx,
		cancel:    cancel,
	}

	// 账本资金安全防线（演示值，生产按资产风险配置）。
	ledgerSvc.SetHotWalletCap("USDT", 150000)
	ledgerSvc.SetHotWalletCap("ETH", 50)
	ledgerSvc.SetHotWalletCap("BTC", 5)
	ledgerSvc.SetWithdrawHoldPeriod(30 * time.Second)
	ledgerSvc.SetDailyWithdrawLimit("USDT", 50000)
	ledgerSvc.SetDailyWithdrawLimit("ETH", 10)
	ledgerSvc.SetDailyWithdrawLimit("BTC", 1)
	ledgerSvc.SetAddressVerifyPeriod(10 * time.Second)
	ledgerSvc.EnableRiskEngine(true, true)
	ledgerSvc.SetRiskThresholds(60*time.Second, 30000, 5, 3)
	ledgerSvc.SetReconcileAlertHook(func(dev map[string]float64) {
		log.Warn("LEDGER_IMBALANCE detected by reconciler", zap.Any("deviation", dev))
	})
	ledgerSvc.StartReconciler(15 * time.Second)

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
		return bal
	})
	s.liquidator.SetADLCallback(func(ev futures.ADLEvent) {
		ref := fmt.Sprintf("adl:%d:%s", ev.UserID, ev.Symbol)
		if err := ledgerSvc.DebitAvailable(ev.UserID, "USDT", ev.ProfitCovered, "adl", ref); err != nil {
			log.Error("adl debit failed", zap.Int64("user", ev.UserID), zap.Error(err))
		}
		if err := ledgerSvc.CreditAvailable(ledger.SysInsurance, "USDT", ev.ProfitCovered, "adl", ref); err != nil {
			log.Error("adl insurance credit failed", zap.Error(err))
		}
		s.hub.Broadcast(ev.Symbol, ginH{"type": "adl", "data": ev})
		log.Warn("auto-deleveraging",
			zap.Int64("user", ev.UserID), zap.String("side", sideName(ev.Side)),
			zap.Float64("reduced", ev.ReducedSize), zap.Float64("profit_covered", ev.ProfitCovered))
	})
	s.liquidator.SetDeficitPayer(func(deficit float64) {
		ref := fmt.Sprintf("deficit:%d", time.Now().UnixNano())
		_ = ledgerSvc.DebitAvailable(ledger.SysInsurance, "USDT", deficit, "liquidation_deficit", ref)
		_ = ledgerSvc.CreditAvailable(ledger.SysLiquidationLoss, "USDT", deficit, "liquidation_deficit", ref)
		log.Warn("liquidation deficit paid by insurance", zap.Float64("deficit", deficit))
	})
	s.liquidator.SetSocializeCallback(func(shares []futures.SocializedLossEvent) {
		for _, sh := range shares {
			ref := fmt.Sprintf("socialize:%d:%s", sh.UserID, sh.Symbol)
			if err := ledgerSvc.DebitAvailable(sh.UserID, "USDT", sh.Share, "socialized_loss", ref); err != nil {
				log.Error("socialize debit failed", zap.Int64("user", sh.UserID), zap.Error(err))
			}
			if err := ledgerSvc.CreditAvailable(ledger.SysInsurance, "USDT", sh.Share, "socialized_loss", ref); err != nil {
				log.Error("socialize insurance credit failed", zap.Error(err))
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

	// 链上充提网关（模拟）及其事件监听。
	s.chainGateway = settlement.NewMockChainGateway(2, 2*time.Second)
	s.chainGateway.Start()
	s.chainWithdraw = settlement.NewMockWithdrawGateway(2, 2*time.Second)
	s.chainWithdraw.Start()
	s.startChainWatchers()

	// 资金费率结算后台循环。
	go s.fundingLoop()
	// 独立强平扫描循环（与成交解耦，保证无成交时价格击穿也能强平）。
	go s.liqScanLoop()

	return s
}

// startChainWatchers 启动链上充值/提现的确认、回滚事件监听 goroutine。
// 这些 goroutine 随 Server.ctx 取消而退出（网关的 Watch 在 ctx 取消时关闭 channel）。
func (s *Server) startChainWatchers() {
	// 充值确认入账。
	go func() {
		ch, err := s.chainGateway.Watch(s.ctx)
		if err != nil {
			s.log.Error("chain gateway watch failed", zap.Error(err))
			return
		}
		for ev := range ch {
			if err := s.ledgerSvc.ReceiveOnChain(ev.UserID, ev.Asset, ev.Amount, ev.TxHash); err != nil {
				s.log.Error("on-chain credit failed", zap.String("tx", ev.TxHash), zap.Error(err))
				continue
			}
			s.log.Info("on-chain deposit credited",
				zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
				zap.Float64("amount", ev.Amount), zap.String("tx", ev.TxHash))
			s.hub.Broadcast("SYS", ginH{"type": "chain_deposit", "data": ev})
		}
	}()

	// 充值孤块/重组回滚。
	go func() {
		rch, err := s.chainGateway.WatchRollback(s.ctx)
		if err != nil {
			s.log.Error("chain gateway watch rollback failed", zap.Error(err))
			return
		}
		for ev := range rch {
			badDebt, err := s.ledgerSvc.ReverseOnChain(ev.UserID, ev.Asset, ev.Amount, ev.TxHash)
			if err != nil {
				s.log.Error("on-chain rollback failed", zap.String("tx", ev.TxHash), zap.Error(err))
				continue
			}
			if badDebt > 0 {
				s.log.Error("on-chain deposit reverted with BAD DEBT (user already spent funds)",
					zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
					zap.Float64("amount", ev.Amount), zap.Float64("bad_debt", badDebt),
					zap.String("tx", ev.TxHash))
			} else {
				s.log.Warn("on-chain deposit reverted (orphan block)",
					zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
					zap.Float64("amount", ev.Amount), zap.String("tx", ev.TxHash))
			}
			s.hub.Broadcast("SYS", ginH{"type": "chain_rollback", "data": ev, "bad_debt": badDebt})
		}
	}()

	// 提现确认划出 / 失败回退。
	go func() {
		wch, err := s.chainWithdraw.WatchWithdraw(s.ctx)
		if err != nil {
			s.log.Error("chain withdraw watch failed", zap.Error(err))
			return
		}
		for ev := range wch {
			total := ev.Amount + ev.Fee
			switch ev.Status {
			case settlement.WithdrawCredited:
				if err := s.ledgerSvc.SettleWithdraw(ev.UserID, ev.Asset, ev.Amount, ev.Fee, ev.TxHash); err != nil {
					s.log.Error("withdraw settle failed", zap.String("tx", ev.TxHash), zap.Error(err))
					continue
				}
				s.log.Info("on-chain withdraw settled",
					zap.Int64("user", ev.UserID), zap.String("asset", ev.Asset),
					zap.Float64("amount", ev.Amount), zap.Float64("fee", ev.Fee),
					zap.String("tx", ev.TxHash))
				s.hub.Broadcast("SYS", ginH{"type": "chain_withdraw", "data": ev})
			case settlement.WithdrawFailed:
				if err := s.ledgerSvc.UnfreezeWithdraw(ev.UserID, ev.Asset, total); err != nil {
					s.log.Error("withdraw rollback failed", zap.String("tx", ev.TxHash), zap.Error(err))
					continue
				}
				s.log.Warn("on-chain withdraw failed, rolled back",
					zap.Int64("user", ev.UserID), zap.String("tx", ev.TxHash))
				s.hub.Broadcast("SYS", ginH{"type": "chain_withdraw_failed", "data": ev})
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
		for ev := range wrh {
			total := ev.Amount + ev.Fee
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
				zap.Float64("amount", ev.Amount), zap.Float64("fee", ev.Fee),
				zap.String("tx", ev.TxHash))
			s.hub.Broadcast("SYS", ginH{"type": "chain_withdraw_rollback", "data": ev})
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
	mc.UpdateContractPrice(t.Price)
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
	if ev.Partial {
		if ev.Mode == futures.Cross {
			if ev.Realized >= 0 {
				_ = s.ledgerSvc.Freeze(ev.UserID, "USDT", ev.Realized)
			} else {
				_ = s.ledgerSvc.Unfreeze(ev.UserID, "USDT", -ev.Realized)
			}
		} else {
			_ = s.ledgerSvc.Unfreeze(ev.UserID, "USDT", ev.Margin)
			if ev.Realized >= 0 {
				_ = s.ledgerSvc.CreditAvailable(ev.UserID, "USDT", ev.Realized, "partial_liq", ref)
			} else {
				_ = s.ledgerSvc.DebitAvailable(ev.UserID, "USDT", -ev.Realized, "partial_liq", ref)
			}
		}
		s.log.Info("partial liquidation",
			zap.Int64("user", ev.UserID), zap.String("mode", modeName(ev.Mode)),
			zap.Float64("closed", ev.Size), zap.Float64("remaining", ev.RemainingSize),
			zap.Float64("realized", ev.Realized))
		return
	}

	_ = s.ledgerSvc.Unfreeze(ev.UserID, "USDT", ev.Margin)
	if err := s.ledgerSvc.DebitAvailable(ev.UserID, "USDT", ev.Margin, "liquidation", ref); err != nil {
		s.log.Error("liquidation debit failed", zap.Error(err))
	}
	if err := s.ledgerSvc.CreditAvailable(ledger.SysInsurance, "USDT", ev.Margin, "liquidation", ref); err != nil {
		s.log.Error("insurance credit failed", zap.Error(err))
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
	o := &matching.Order{
		UserID: userID, // taker 标识为被强平用户（其持仓正被平仓）
		Side:   ms,
		Price:  0, // 市价单
		Qty:    qty,
		Time:   time.Now().UnixNano(),
	}
	trades, fully := s.matcher.MatchNow(symbol, o, false)
	var filled, notional float64
	for _, t := range trades {
		filled += t.Qty
		notional += t.Price * t.Qty
	}
	if !fully {
		rem := qty - filled
		if rem > 1e-9 {
			// 保险基金兜底成交：maker 为 SysLiquidationLoss（破产价/标记价），保证强平必定完成。
			trades = append(trades, matching.Trade{
				Price:     mark,
				Qty:       rem,
				TakerID:   userID,
				MakerID:   ledger.SysLiquidationLoss,
				TakerSide: ms,
			})
			filled = qty
			notional += mark * rem
		}
	}
	avg := mark
	if filled > 1e-9 {
		avg = notional / filled
	}
	return futures.LiquidationFill{Filled: filled, AvgPrice: avg, Trades: len(trades)}
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
			}
		}
	}
}

// broadcastLiquidations 广播强平事件（onTrade 与 liqScanLoop 共用）：记录审计日志并推送 WS。
func (s *Server) broadcastLiquidations(evs []futures.LiquidationEvent) {
	for _, ev := range evs {
		s.log.Warn("liquidation",
			zap.Int64("user", ev.UserID),
			zap.String("side", sideName(ev.Side)),
			zap.Float64("liq_price", ev.LiqPrice),
			zap.Float64("fee", ev.Fee),
			zap.Bool("partial", ev.Partial))
		s.hub.Broadcast(ev.Symbol, ginH{"type": "liquidation", "data": ev})
	}
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
					switch {
					case p.Payment < 0:
						pay := -p.Payment
						if s.liquidator.ModeOf(sym, p.UserID) == futures.Cross {
							_ = s.ledgerSvc.Unfreeze(p.UserID, "USDT", pay)
							s.liquidator.AdjustCrossBalance(sym, p.UserID, -pay)
						}
						if err := s.ledgerSvc.Transfer(p.UserID, ledger.SysFundingPool, "USDT", pay, "funding", ref); err != nil {
							s.log.Error("funding debit failed", zap.Int64("user", p.UserID), zap.Error(err))
						}
					case p.Payment > 0:
						if err := s.ledgerSvc.Transfer(ledger.SysFundingPool, p.UserID, "USDT", p.Payment, "funding", ref); err != nil {
							s.log.Error("funding credit failed", zap.Int64("user", p.UserID), zap.Error(err))
						}
						if s.liquidator.ModeOf(sym, p.UserID) == futures.Cross {
							s.liquidator.AdjustCrossBalance(sym, p.UserID, p.Payment)
							_ = s.ledgerSvc.Freeze(p.UserID, "USDT", p.Payment)
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
