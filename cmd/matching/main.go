package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/persist"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/ws"
)

// cmd/matching 是撮合引擎的独立部署形态，演示「多实例单写者」：
//   - 以 MySQL 为共享后端（ce_matching_* 表）时，多个进程同时启动，仅一个为 leader，
//     负责写订单簿；其余为 follower（standby），leader 租约过期后竞争接管，接管时从
//     快照+WAL 恢复，不丢单。
//   - 未配置 MySQL 时使用内存 Store，退化为单实例（仍走 leader 选举逻辑，仅一个进程有效）。
//
// 这是撮合引擎支持多实例部署的权威形态，也是全交易所的「单一匹配权威」：
// spot/futures 等服务改为调用本服务的 HTTP(+WS) API（见 DEVELOPMENT_TASKS §17/§18），
// 从而把「匹配」收敛为单一可水平容灾的组件。本服务对外暴露：
//   - POST /order      提交订单（leader-only），支持 user_id（强平/记账溯源）
//   - POST /cancel     撤销订单（leader-only）
//   - POST /match-now  同步撮合（leader-only），用于强平，返回真实成交
//   - GET  /depth      订单簿深度（leader-only）
//   - GET  /ws?symbol= 行情 WebSocket：按 symbol 推送成交与深度（symbol 空=全部）
//   - GET  /health     健康与 leader 状态
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN (overrides config); empty=in-memory")
	addr := flag.String("addr", ":8085", "HTTP listen addr")
	nodeID := flag.String("node-id", "", "unique node id (default NODE_ID env or hostname-pid)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 按配置覆盖 Kafka 协议协商版本（无 -tags kafka 时为空操作）。
	mq.SetKafkaVersion(cfg.Kafka.Version)

	// 交易对集合：优先 config.Matching.Symbols，否则默认覆盖现货与合约永续。
	symbols := cfg.Matching.Symbols
	if len(symbols) == 0 {
		symbols = []string{"BTC_USDT", "ETH_USDT", "BTC_USDT_PERP", "ETH_USDT_PERP"}
	}

	// 选择 Store：配置了 DSN 则用 MySQL（跑迁移），否则内存。
	var store matching.Store
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn != "" {
		ms, merr := persist.NewMySQLStore(dsn)
		if merr != nil {
			log.Fatal("matching mysql store unavailable", zap.String("dsn", dsn), zap.Error(merr))
		}
		if err := migrate.New(ms.DB(), persist.Migrations()).Up(); err != nil {
			log.Fatal("matching migration failed", zap.Error(err))
		}
		store = ms
		log.Info("matching store: mysql", zap.String("dsn", dsn))
		defer func() { _ = ms.Close() }()
	} else {
		store = persist.NewMemStore()
		log.Info("matching store: in-memory (single instance)")
	}

	id := *nodeID
	if id == "" {
		id = os.Getenv("NODE_ID")
	}
	if id == "" {
		h, _ := os.Hostname()
		id = h + "-" + itoa(os.Getpid())
	}

	snapInterval := time.Duration(cfg.Matching.SnapshotIntervalSec) * time.Second
	ttl := time.Duration(cfg.Matching.LeaderTTLSec) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Second
	}

	// 行情广播中心：撮合产生的成交/深度经 onTrade/onBook 推送给订阅 WS 的客户端
	// （spot/futures 的 matching.Client 据此驱动各自的业务逻辑与前端行情）。
	hub := ws.NewHub()

	// 成交流/深度流发布器：配置了 Kafka brokers 且以 -tags kafka 构建时投递到 Kafka
	// （exchange.trades / market.depth），否则退回内存发布器（无消费方，仅不阻断撮合）。
	pub := mq.NewPublisher(cfg.Kafka.Brokers, cfg.Kafka.TradeTopic, cfg.Kafka.DepthTopic, nil)
	defer func() { _ = pub.Close() }()

	// 深度节流发布所需的状态（符号表 + 锁），由 onBook 标记、节流 goroutine 消费。
	depthMu := sync.Mutex{}
	pendingDepth := map[string]bool{}
	const depthTopN = 20
	const depthPublishInterval = 200 * time.Millisecond

	var e *matching.Engine
	e = matching.NewEngine(
		func(symbol string, t matching.Trade) {
			hub.Broadcast(symbol, gin.H{"type": "trade", "symbol": symbol, "data": t})
			// 成交同时发布到 Kafka 成交流（exchange.trades），供清算/行情/风控消费。
			takerSide := "buy"
			if t.TakerSide == matching.Sell {
				takerSide = "sell"
			}
			if err := pub.PublishTrade(context.Background(), mq.TradeEvent{
				Symbol:    symbol,
				Price:     t.Price,
				Qty:       t.Qty,
				TakerID:   t.TakerID,
				MakerID:   t.MakerID,
				TakerSide: takerSide,
				Ts:        time.Now().UnixMilli(),
			}); err != nil {
				log.Warn("publish trade failed", zap.String("symbol", symbol), zap.Error(err))
			}
		},
		func(symbol string) {
			bids, asks, ok := e.Depth(symbol)
			if !ok {
				return
			}
			hub.Broadcast(symbol, gin.H{
				"type":  "depth",
				"symbol": symbol,
				"data":  gin.H{"bids": bids, "asks": asks},
			})
			// 标记该交易对深度待发布（由节流 goroutine 聚合后发往 Kafka）。
			depthMu.Lock()
			pendingDepth[symbol] = true
			depthMu.Unlock()
		},
	)
	e.UseStore(store, id, snapInterval)
	for _, s := range symbols {
		e.Register(s) // 预注册空簿，保证即使无活动也有交易对可服务
	}

	// 上下文：驱动深度节流 goroutine 与 leader 选举循环的退出。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 深度节流发布：onBook 仅标记待发布 symbol，本 goroutine 按固定间隔聚合最新深度
	// 发送到 market.depth（避免每笔订单都发全量快照，见 KAFKA_TOPICS.md「深度快照（节流）」）。
	// 必须在 e 注册完成且 ctx 声明后启动。
	go func() {
		ticker := time.NewTicker(depthPublishInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				depthMu.Lock()
				syms := make([]string, 0, len(pendingDepth))
				for s := range pendingDepth {
					syms = append(syms, s)
					delete(pendingDepth, s)
				}
				depthMu.Unlock()
				for _, sym := range syms {
					bids, asks, ok := e.Depth(sym)
					if !ok {
						continue
					}
					ev := mq.DepthEvent{
						Symbol: sym,
						Bids:   aggregateDepth(bids, depthTopN, true),
						Asks:   aggregateDepth(asks, depthTopN, false),
						Ts:     time.Now().UnixMilli(),
					}
					if err := pub.PublishDepth(context.Background(), ev); err != nil {
						log.Warn("publish depth failed", zap.String("symbol", sym), zap.Error(err))
					}
				}
			}
		}
	}()

	var isLeader atomic.Bool

	// leader 选举循环：tryAcquire → recover+snapshot / 失去则 Reset 进入 standby。
	go func() {
		ticker := time.NewTicker(ttl / 2)
		defer ticker.Stop()
		leader := false
		var snapCancel context.CancelFunc
		for {
			select {
			case <-ctx.Done():
				if leader {
					_ = store.ReleaseLeader(ctx, id)
				}
				if snapCancel != nil {
					snapCancel()
				}
				return
			case <-ticker.C:
				ok, lerr := store.TryAcquireLeader(ctx, id, ttl)
				if lerr != nil {
					log.Warn("leader election error", zap.Error(lerr))
					continue
				}
				switch {
				case ok && !leader:
					if rerr := e.Recover(ctx); rerr != nil {
						log.Error("recover failed", zap.Error(rerr))
						continue
					}
					for _, s := range symbols {
						e.Register(s)
					}
					leader = true
					isLeader.Store(true)
					var snapCtx context.Context
					snapCtx, snapCancel = context.WithCancel(ctx)
					go e.SnapshotLoop(snapCtx)
					log.Info("became leader", zap.String("node", id))
				case !ok && leader:
					leader = false
					isLeader.Store(false)
					if snapCancel != nil {
						snapCancel()
						snapCancel = nil
					}
					e.Reset()
					log.Warn("lost leadership, entering standby", zap.String("node", id))
				case leader:
					if renewed, rerr := store.RenewLeader(ctx, id, ttl); rerr != nil || !renewed {
						leader = false
						isLeader.Store(false)
						if snapCancel != nil {
							snapCancel()
							snapCancel = nil
						}
						e.Reset()
						log.Warn("leadership expired, entering standby", zap.String("node", id))
					}
				}
			}
		}
	}()

	r := gin.New()
	r.Use(middleware.Common(log, cfg)...)

	leaderGuard := func(c *gin.Context) {
		if !isLeader.Load() {
			response.Error(c, http.StatusServiceUnavailable, 503, "not leader")
			c.Abort()
			return
		}
	}

	r.POST("/order", func(c *gin.Context) {
		leaderGuard(c)
		if c.IsAborted() {
			return
		}
		var req struct {
			Symbol string  `json:"symbol"`
			Side   string  `json:"side"`
			Price  float64 `json:"price"`
			Qty    float64 `json:"qty"`
			UserID int64   `json:"user_id"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			response.Error(c, 400, 400, "bad request")
			return
		}
		if req.Qty <= 0 {
			response.Error(c, 400, 400, "qty must be positive")
			return
		}
		side := matching.Buy
		if req.Side == "sell" {
			side = matching.Sell
		}
		o := &matching.Order{
			UserID: req.UserID,
			Side:   side,
			Price:  req.Price,
			Qty:    req.Qty,
			Time:   time.Now().UnixNano(),
		}
		if !e.Submit(req.Symbol, o) {
			response.Error(c, 400, 400, "unknown symbol")
			return
		}
		response.JSON(c, gin.H{"order_id": o.ID, "status": "accepted"})
	})

	r.POST("/cancel", func(c *gin.Context) {
		leaderGuard(c)
		if c.IsAborted() {
			return
		}
		var req struct {
			Symbol  string `json:"symbol"`
			OrderID int64  `json:"order_id"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			response.Error(c, 400, 400, "bad request")
			return
		}
		canceled := e.Cancel(req.Symbol, req.OrderID)
		response.JSON(c, gin.H{"symbol": req.Symbol, "order_id": req.OrderID, "canceled": canceled})
	})

	// /match-now：同步撮合一笔订单并返回真实成交（用于强平）。rest=false 时市价单
	// 未成交部分直接丢弃（由调用方以保险基金兜底成交）。注意本路径不触发 onTrade，
	// 因此强平成交不会经 WS 重复广播给上游（避免重入），其记账由调用方同步处理。
	r.POST("/match-now", func(c *gin.Context) {
		leaderGuard(c)
		if c.IsAborted() {
			return
		}
		var req struct {
			Symbol string  `json:"symbol"`
			Side   string  `json:"side"`
			Price  float64 `json:"price"`
			Qty    float64 `json:"qty"`
			UserID int64   `json:"user_id"`
			Rest   bool    `json:"rest"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			response.Error(c, 400, 400, "bad request")
			return
		}
		if req.Qty <= 0 {
			response.Error(c, 400, 400, "qty must be positive")
			return
		}
		side := matching.Buy
		if req.Side == "sell" {
			side = matching.Sell
		}
		o := &matching.Order{
			UserID: req.UserID,
			Side:   side,
			Price:  req.Price,
			Qty:    req.Qty,
			Time:   time.Now().UnixNano(),
		}
		trades, fully := e.MatchNow(req.Symbol, o, req.Rest)
		response.JSON(c, gin.H{
			"symbol":       req.Symbol,
			"trades":       trades,
			"filled":       o.Filled,
			"fully_filled": fully,
		})
	})

	r.GET("/depth", func(c *gin.Context) {
		leaderGuard(c)
		if c.IsAborted() {
			return
		}
		symbol := c.Query("symbol")
		bids, asks, ok := e.Depth(symbol)
		if !ok {
			response.Error(c, 400, 400, "unknown symbol")
			return
		}
		response.JSON(c, gin.H{"symbol": symbol, "bids": bids, "asks": asks})
	})

	// 以下查询接口读取「订单/成交登记簿」，该簿仅在 leader 上维护，故统一经 leaderGuard
	// 路由到 leader（与 /depth 一致）。user_id 由已鉴权的上游服务透传，本服务不再二次鉴权。

	// GET /orders?user_id=&symbol=&status=&limit=：查询指定用户的订单（可按 symbol/status 过滤）。
	r.GET("/orders", func(c *gin.Context) {
		leaderGuard(c)
		if c.IsAborted() {
			return
		}
		userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
		symbol := c.Query("symbol")
		status := c.Query("status")
		limit, _ := strconv.Atoi(c.Query("limit"))
		response.JSON(c, e.ListOrders(userID, symbol, status, limit))
	})

	// GET /orders/:id：按订单 ID 查详情；不存在返回 404。
	r.GET("/orders/:id", func(c *gin.Context) {
		leaderGuard(c)
		if c.IsAborted() {
			return
		}
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if v, ok := e.GetOrder(id); ok {
			response.JSON(c, v)
		} else {
			response.Error(c, http.StatusNotFound, 404, "not found")
		}
	})

	// GET /trades?user_id=&symbol=&limit=：查询指定用户的成交流水。
	r.GET("/trades", func(c *gin.Context) {
		leaderGuard(c)
		if c.IsAborted() {
			return
		}
		userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
		symbol := c.Query("symbol")
		limit, _ := strconv.Atoi(c.Query("limit"))
		response.JSON(c, e.ListTrades(userID, symbol, limit))
	})

	// /ws：行情 WebSocket。symbol 空表示订阅全部交易对；否则仅该 symbol。
	r.GET("/ws", func(c *gin.Context) {
		hub.Handle(c.Writer, c.Request)
	})

	r.GET("/health", func(c *gin.Context) {
		leader, _ := store.IsLeader(ctx, id)
		response.JSON(c, gin.H{
			"node":      id,
			"leader":    leader,
			"is_leader": isLeader.Load(),
			"symbols":   symbols,
			"time":      time.Now().Unix(),
		})
	})

	log.Info("matching service starting", zap.String("addr", *addr), zap.String("node", id), zap.Strings("symbols", symbols))
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
		_ = store.ReleaseLeader(context.Background(), id)
		_ = log.Sync()
		os.Exit(0)
	}()

	if err := cfg.Listen(r, *addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// aggregateDepth 把订单簿层级聚合为深度行（top-N）：买盘取价格最高 N 档、卖盘取价格最低 N 档，
// 每档 volume 为该档各订单 Qty 之和。用于发布到 market.depth 的节流快照。
func aggregateDepth(levels []matching.Level, n int, isBid bool) []mq.DepthLevel {
	ls := make([]matching.Level, len(levels))
	copy(ls, levels)
	sort.Slice(ls, func(i, j int) bool {
		if isBid {
			return ls[i].Price > ls[j].Price
		}
		return ls[i].Price < ls[j].Price
	})
	if len(ls) > n {
		ls = ls[:n]
	}
	out := make([]mq.DepthLevel, 0, len(ls))
	for _, l := range ls {
		var v float64
		for _, o := range l.Orders {
			v += o.Qty
		}
		out = append(out, mq.DepthLevel{Price: l.Price, Volume: v})
	}
	return out
}
