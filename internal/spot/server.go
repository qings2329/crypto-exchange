package spot

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/client"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/coldlar/crypto-exchange/internal/ws"
)

// matcherClient 是 spot 对撮合能力的依赖抽象（接口），便于单测注入假实现。
// client.Client 自然满足该接口；生产由 cmd/spot 传入具体 *client.Client。
type matcherClient interface {
	Submit(symbol string, o *matching.Order) bool
	CancelOrder(symbol string, orderID int64) (bool, error)
	GetOrder(orderID int64) (matching.OrderView, bool)
	ListOrders(userID int64, symbol, status string, limit int) []matching.OrderView
	ListTrades(userID int64, symbol string, limit int) []matching.TradeView
	Depth(symbol string) (bids, asks []matching.Level, ok bool)
	Watch(ctx context.Context, symbols []string, onTrade func(client.TradeEvent), onDepth func(client.DepthEvent)) error
}

// Server 聚合现货交易服务运行所需的依赖与生命周期。
//
// 多实例收敛（见 DEVELOPMENT_TASKS §18）：现货不再持有进程内撮合引擎，而是改为调用
// 独立的 cmd/matching 服务（matching.Client 实现 matching.Matcher）。匹配权威唯一，
// 订单簿不再随现货进程分裂；成交流与深度由 cmd/matching 经 WebSocket 推送，本服务
// 仅负责转发到前端行情 hub。
//
// 资金闭环（本里程碑新增）：下单前在 ledger 预冻结买方计价资产 / 卖方基础资产；每笔成交
// 经 settleFill 解冻已成交部分并转账（买方付计价、卖方付基础）；撤单释放剩余冻结。
//
// 资金安全（F1–F5 边界审查整改）：
//   - F4 身份必须来自鉴权 token，拒绝请求体 user_id 冒充。
//   - F1 下单幂等（client_oid 去重，避免重试双冻）+ 成交重放去重（settledRefs，避免双付）。
//   - F1 并发成交串行化（settleFill 全程持 freezeMu），消除 freezeRec 竞态。
//   - F5 拒绝 price<=0 订单；结算纵深拦截零/负额转账。
//   - F2 预冻结额以 settlement.AssetAmount 跟踪，结算钳位到真实剩余，消除浮点漂移/残留。
//   - F3 两腿结算经账本 Batch 原子执行（解冻+转账整体回滚），提供 admin/reconcile 对账。
type Server struct {
	log    *zap.Logger
	client matcherClient
	hub    *ws.Hub

	ledgerSvc *ledger.Ledger // 现货自有复式记账账本（与合约服务各自独立实例，见计划说明）

	freezeMu    sync.Mutex
	openOrders  map[int64]*freezeRec // orderID -> 预冻结记录，供成交递减与撤单释放
	clientOIDMap map[string]int64    // "uid:client_oid" -> orderID（下单幂等，避免重试双冻）
	settledRefs map[string]bool      // 成交去重键 -> 已结算（避免重放双付）

	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer 装配现货交易服务：创建撮合客户端并订阅行情 WebSocket，转发到前端 hub。
func NewServer(ledgerSvc *ledger.Ledger, cfg *config.Config, log *zap.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		log:          log,
		client:       client.New(cfg.Matching.URL),
		hub:          ws.NewHub(),
		ledgerSvc:    ledgerSvc,
		openOrders:   make(map[int64]*freezeRec),
		clientOIDMap: make(map[string]int64),
		settledRefs:  make(map[string]bool),
		ctx:          ctx,
		cancel:       cancel,
	}

	// 订阅 cmd/matching 的行情流（成交 + 深度），转发到本地前端 hub；成交同时驱动记账。
	go func() {
		if err := s.client.Watch(ctx, []string{"BTC_USDT", "ETH_USDT"},
			func(ev client.TradeEvent) {
				s.hub.Broadcast(ev.Symbol, gin.H{"type": "trade", "data": ev.Trade})
				// 成交记账：异常被吞掉，绝不影响行情推送与 WS 读取循环。
				func() {
					defer func() {
						if r := recover(); r != nil {
							s.log.Error("spot settleFill panic", zap.Any("recover", r))
						}
					}()
					if err := s.settleFill(ev.Symbol, ev.Trade); err != nil {
						s.log.Warn("spot settleFill failed", zap.String("symbol", ev.Symbol), zap.Error(err))
					}
				}()
			},
			func(ev client.DepthEvent) {
				s.hub.Broadcast(ev.Symbol, gin.H{
					"type": "depth",
					"data": gin.H{"bids": aggregate(ev.Bids), "asks": aggregate(ev.Asks)},
				})
			}); err != nil && ctx.Err() == nil {
			s.log.Warn("spot matching watch exited", zap.Error(err))
		}
	}()

	return s
}

// RegisterRoutes 注册现货交易服务 HTTP 路由。
// 下单/撤单接口受鉴权保护；行情深度与 WebSocket 行情为公开端点（豁免鉴权），由网关/前端直接消费。
func (s *Server) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	r.Use(middleware.AuthWithSkips(verifier, "/api/v1/spot/depth", "/api/v1/spot/ws"))
	r.POST("/api/v1/spot/order", s.handleOrder)
	r.POST("/api/v1/spot/cancel", s.handleCancel)
	r.GET("/api/v1/spot/depth", s.handleDepth)
	r.GET("/api/v1/spot/ws", s.handleWS)
	// 用户侧订单管理：仅返回鉴权用户本人的订单/成交（按 token 中的 uid 过滤）。
	r.GET("/api/v1/spot/orders", s.handleOrders)
	r.GET("/api/v1/spot/orders/:id", s.handleOrderDetail)
	r.GET("/api/v1/spot/trades", s.handleTrades)
	// 资金对账：仅管理员。
	r.GET("/api/v1/spot/admin/reconcile", middleware.AdminGuard(), s.handleReconcile)
}

// handleOrder 提交一笔现货订单（买/卖），经 cmd/matching 撮合，并在撮合前预冻结资金。
// F4：身份来自鉴权 token（忽略请求体 user_id）。F5：拒绝 price<=0。F1：client_oid 幂等。
func (s *Server) handleOrder(c *gin.Context) {
	var req struct {
		Symbol    string  `json:"symbol"`
		UserID    int64   `json:"user_id"` // 已废弃：身份必须来自 token（F4）
		Side      string  `json:"side"`
		Price     float64 `json:"price"`
		Qty       float64 `json:"qty"`
		IsMargin  bool    `json:"is_margin"`          // 杠杆现货单（借币后下单）
		Leverage  float64 `json:"leverage,omitempty"` // 杠杆倍数（is_margin 时有效）
		ClientOID string  `json:"client_oid"`          // 下单幂等键（F1）
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	if req.Qty <= 0 {
		response.Error(c, 400, 400, "qty must be positive")
		return
	}
	if req.Price <= 0 { // F5：零/负价会被结算视为白嫖（付 0 计价收基础资产），必须拦截。
		response.Error(c, 400, 400, "price must be positive")
		return
	}
	side := matching.Buy
	if req.Side == "sell" {
		side = matching.Sell
	}
	if _, _, ok := splitSymbol(req.Symbol); !ok {
		response.Error(c, 400, 400, "unsupported symbol")
		return
	}

	o := &matching.Order{
		UserID: uid, // F4：身份来自 token。
		Side:   side,
		Price:  req.Price,
		Qty:    req.Qty,
		Time:   time.Now().UnixNano(),
		Market: "spot",
		// 杠杆现货单标记为 IsMargin，倍数透传；用于订单管理按杠杆过滤。
		IsMargin: req.IsMargin,
		Leverage: req.Leverage,
	}

	// F1 下单幂等 + 防并发双冻：将「幂等键检查 → 预冻结 → 提交 → 登记」整体置于 freezeMu
	// 临界区内，杜绝同 client_oid 在"检查通过"与"实际冻结"之间的竞态窗口。此前两段不在同一
	// 锁内，并发重试会各自通过检查并重复 Freeze，导致用户资金被双重冻结。
	s.freezeMu.Lock()
	if req.ClientOID != "" {
		key := fmt.Sprintf("%d:%s", uid, req.ClientOID)
		if oid, exists := s.clientOIDMap[key]; exists {
			s.freezeMu.Unlock()
			response.JSON(c, gin.H{"order_id": oid, "status": "accepted", "idempotent": true})
			return
		}
	}
	// 预冻结（临界区内执行，与提交/登记原子，避免成交事件早到漏结算与双冻）。
	rec, err := s.reserveOnOpen(uid, side, req.Price, req.Qty, req.Symbol)
	if err != nil {
		s.freezeMu.Unlock()
		response.Error(c, 400, 400, "insufficient balance: "+err.Error())
		return
	}
	if !s.client.Submit(req.Symbol, o) {
		// 撮合不可用，回滚预冻结，避免资金被错误锁定。
		s.releaseRemaining(rec)
		s.freezeMu.Unlock()
		response.Error(c, 400, 400, "unknown symbol or matching unavailable")
		return
	}
	s.openOrders[o.ID] = rec
	if req.ClientOID != "" {
		s.clientOIDMap[fmt.Sprintf("%d:%s", uid, req.ClientOID)] = o.ID
	}
	s.freezeMu.Unlock()

	response.JSON(c, gin.H{"order_id": o.ID, "status": "accepted"})
}

// reserveOnOpen 下单前预冻结资金，返回（尚未绑定 orderID 的）预冻结记录。
// 冻结额以 settlement.AssetAmount 计算并跟踪（F2）。price>0 已由调用方校验。
func (s *Server) reserveOnOpen(userID int64, side matching.Side, price, qty float64, symbol string) (*freezeRec, error) {
	base, quote, ok := splitSymbol(symbol)
	if !ok {
		return nil, fmt.Errorf("unsupported symbol %s", symbol)
	}
	rec := &freezeRec{user: userID, side: side, symbol: symbol, base: base, quote: quote}
	var asset string
	var amt settlement.AssetAmount
	switch {
	case side == matching.Buy:
		asset = quote
		amt = settlement.AssetAmountFromFloat(price*qty, settlement.AssetDecimalsByName(asset))
	case side == matching.Sell:
		asset = base
		amt = settlement.AssetAmountFromFloat(qty, settlement.AssetDecimalsByName(asset))
	}
	if !amt.IsZero() {
		if err := s.ledgerSvc.Freeze(userID, asset, amt); err != nil {
			return nil, err
		}
	}
	if side == matching.Buy {
		rec.frozenQuote = amt
	} else {
		rec.frozenBase = amt
	}
	return rec, nil
}

// releaseRemaining 释放一条预冻结记录中尚未成交的冻结资金（按资产区分冻结维度）。
func (s *Server) releaseRemaining(rec *freezeRec) {
	if rec == nil {
		return
	}
	if !rec.frozenQuote.IsZero() {
		_ = s.ledgerSvc.Unfreeze(rec.user, rec.quote, rec.frozenQuote)
	}
	if !rec.frozenBase.IsZero() {
		_ = s.ledgerSvc.Unfreeze(rec.user, rec.base, rec.frozenBase)
	}
}

// handleCancel 撤销一笔现货订单，并释放其尚未成交的预冻结资金。
// F4：释放/转发前校验订单归属本人（否则 403，不释放、不转发）；rec 缺失时向撮合引擎核验归属。
func (s *Server) handleCancel(c *gin.Context) {
	var req struct {
		Symbol  string `json:"symbol"`
		OrderID int64  `json:"order_id"`
		UserID  int64  `json:"user_id"` // 已废弃：身份来自 token（F4）
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}

	// F4 归属校验：本地记录存在时确认归属本人。
	s.freezeMu.Lock()
	rec, exists := s.openOrders[req.OrderID]
	if exists {
		if rec.user != uid { // 非本人：拒绝，不释放、不转发撤单。
			s.freezeMu.Unlock()
			response.Error(c, 403, 403, "forbidden")
			return
		}
		s.releaseRemaining(rec)
		delete(s.openOrders, req.OrderID)
		s.cleanupClientOIDLocked(req.OrderID)
		s.freezeMu.Unlock()
	} else {
		s.freezeMu.Unlock()
		// 本地记录缺失（已成交清理/从未见）：向撮合引擎核验归属，避免越权撤他人订单。
		if v, ok2 := s.client.GetOrder(req.OrderID); ok2 {
			if v.UserID != uid {
				response.Error(c, 403, 403, "forbidden")
				return
			}
		}
	}

	canceled, _ := s.client.CancelOrder(req.Symbol, req.OrderID)
	response.JSON(c, gin.H{"symbol": req.Symbol, "order_id": req.OrderID, "canceled": canceled})
}

// settleFill 结算一笔现货成交：买方支付计价资产、卖方支付基础资产，均经账本转账。
// 若对应订单有预冻结，则先释放已成交部分再转账，保证「冻结→解冻→划转」资金闭环。
//
// 资金安全整改：全程持 freezeMu 串行（F1 竞态）；成交去重（F1 重放双付）；
// 两腿结算经账本 Batch 原子执行、任一失败整组回滚（F3 原子性）；结算额钳位到真实剩余（F2）；
// 零/负额拦截（F5 纵深）。
func (s *Server) settleFill(symbol string, t matching.Trade) error {
	base, quote, ok := splitSymbol(symbol)
	if !ok {
		return fmt.Errorf("unsupported symbol %s", symbol)
	}
	// F5 纵深：ledger.Transfer 允许 0 转账，必须拦截零/负额，避免"付 0 计价收基础资产"白嫖。
	if t.Price <= 0 || t.Qty <= 0 {
		return fmt.Errorf("invalid trade: price/qty must be positive")
	}
	cost := t.Price * t.Qty

	// 确定买卖方用户（TakerSide 决定哪一侧是买方）。
	var buyer, seller int64
	if t.TakerSide == matching.Buy {
		buyer, seller = t.TakerID, t.MakerID
	} else {
		buyer, seller = t.MakerID, t.TakerID
	}
	ref := tradeRef(symbol, t)

	s.freezeMu.Lock()
	defer s.freezeMu.Unlock()

	// F1 重放去重：已结算则跳过（临界区内检查+置位，保证仅结算一次）。
	if s.settledRefs[ref] {
		return nil
	}

	// 按订单维度查找本方预冻结记录，成交时递减。
	buyRec := s.lookupFreezeLocked(t.TakerOID, t.MakerOID, matching.Buy)
	sellRec := s.lookupFreezeLocked(t.TakerOID, t.MakerOID, matching.Sell)

	// 计价腿：买方支付 quote（先解冻后转账）。
	quoteAmt := settlement.AssetAmountFromFloat(cost, settlement.AssetDecimalsByName(quote))
	if buyRec != nil && quoteAmt.Cmp(buyRec.frozenQuote) > 0 {
		quoteAmt = buyRec.frozenQuote // F2 钳位到真实剩余，消除累计漂移/残留。
	}
	// 基础腿：卖方支付 base（先解冻后转账）。
	baseAmt := settlement.AssetAmountFromFloat(t.Qty, settlement.AssetDecimalsByName(base))
	if sellRec != nil && baseAmt.Cmp(sellRec.frozenBase) > 0 {
		baseAmt = sellRec.frozenBase
	}

	// F3 原子结算：两条腿（解冻+转账）整体经账本 Batch 执行，任一失败即整组回滚，
	// 杜绝"计价腿成功、基础腿失败"造成的部分结算（买方付讫却未收到基础资产、卖方凭空获利）。
	// 两条 Transfer 共用同一 ref，但指纹含 from/to/asset/amount，互不冲突（见 ledger.transferFingerprint）。
	var ops []ledger.Op
	if !quoteAmt.IsZero() {
		if buyRec != nil {
			ops = append(ops, ledger.Op{Kind: ledger.OpUnfreeze, User: buyer, Asset: quote, Amount: quoteAmt})
		}
		ops = append(ops, ledger.Op{Kind: ledger.OpTransfer, From: buyer, To: seller, Asset: quote, Amount: quoteAmt, Biz: "spot_trade", Ref: ref})
	}
	if !baseAmt.IsZero() {
		if sellRec != nil {
			ops = append(ops, ledger.Op{Kind: ledger.OpUnfreeze, User: seller, Asset: base, Amount: baseAmt})
		}
		ops = append(ops, ledger.Op{Kind: ledger.OpTransfer, From: seller, To: buyer, Asset: base, Amount: baseAmt, Biz: "spot_trade", Ref: ref})
	}
	if len(ops) > 0 {
		if err := s.ledgerSvc.Batch(ops); err != nil {
			return fmt.Errorf("settle fill: %w", err)
		}
	}

	// 全部成功：更新本地预冻结记录 + 去重标记，清理终态记录（释放 client_oid 映射，避免无界增长）。
	if buyRec != nil {
		buyRec.frozenQuote = buyRec.frozenQuote.Sub(quoteAmt)
	}
	if sellRec != nil {
		sellRec.frozenBase = sellRec.frozenBase.Sub(baseAmt)
	}
	s.settledRefs[ref] = true
	s.maybeCleanupLocked(t.TakerOID)
	s.maybeCleanupLocked(t.MakerOID)
	return nil
}

// tradeRef 生成成交去重键。注：同对同价同量两笔独立成交理论上同键（极罕见，实践中几乎不可能），
// 此处沿用现有 Trade 字段组合，必要时可改为在 matching.Trade 增加全局唯一 Seq。
func tradeRef(symbol string, t matching.Trade) string {
	return fmt.Sprintf("spot:%s:t%d:m%d:p%v:q%v:b%d:s%d:%v",
		symbol, t.TakerOID, t.MakerOID, t.Price, t.Qty, t.TakerID, t.MakerID, t.TakerSide)
}

// lookupFreezeLocked 在预冻结记录中按订单 ID 与方向查找（taker/maker 任一命中即可）。
// 调用方须已持 freezeMu。
func (s *Server) lookupFreezeLocked(takerOID, makerOID int64, side matching.Side) *freezeRec {
	for _, oid := range []int64{takerOID, makerOID} {
		if rec, ok := s.openOrders[oid]; ok && rec.side == side {
			return rec
		}
	}
	return nil
}

// maybeCleanupLocked 当一笔订单的预冻结已完全释放时，移除其记录并清理 client_oid 映射。
// 调用方须已持 freezeMu。
func (s *Server) maybeCleanupLocked(orderID int64) {
	if orderID == 0 {
		return
	}
	rec, ok := s.openOrders[orderID]
	if !ok {
		return
	}
	if rec.frozenQuote.IsZero() && rec.frozenBase.IsZero() {
		delete(s.openOrders, orderID)
		s.cleanupClientOIDLocked(orderID)
	}
}

// cleanupClientOIDLocked 删除指向指定 orderID 的 client_oid 映射条目。调用方须已持 freezeMu。
func (s *Server) cleanupClientOIDLocked(orderID int64) {
	for k, oid := range s.clientOIDMap {
		if oid == orderID {
			delete(s.clientOIDMap, k)
			return
		}
	}
}

// handleOrders 返回当前用户本人的现货订单列表，可按 symbol/status 过滤、limit 分页。
func (s *Server) handleOrders(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	symbol := c.Query("symbol")
	status := c.Query("status")
	margin := c.Query("margin")
	limit, _ := strconv.Atoi(c.Query("limit"))
	all := s.client.ListOrders(uid, symbol, status, 0)
	orders := make([]matching.OrderView, 0, len(all))
	for _, v := range all {
		if v.Market != "spot" {
			continue
		}
		if !v.MarginMatches(margin) {
			continue
		}
		orders = append(orders, v)
	}
	if limit > 0 && len(orders) > limit {
		orders = orders[:limit]
	}
	response.JSON(c, gin.H{"orders": orders})
}

// handleOrderDetail 返回某笔现货订单详情；仅允许查看本人订单。
func (s *Server) handleOrderDetail(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, 400, 400, "bad order id")
		return
	}
	v, ok2 := s.client.GetOrder(id)
	if !ok2 {
		response.Error(c, 404, 404, "not found")
		return
	}
	if v.UserID != uid {
		response.Error(c, 403, 403, "forbidden")
		return
	}
	response.JSON(c, gin.H{"order": v})
}

// handleTrades 返回当前用户本人的现货成交流水，可按 symbol 过滤、limit 分页。
// 按 market=spot 过滤，仅返回现货成交（合约成交在统一登记簿中需区分）。
func (s *Server) handleTrades(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	symbol := c.Query("symbol")
	margin := c.Query("margin")
	limit, _ := strconv.Atoi(c.Query("limit"))
	all := s.client.ListTrades(uid, symbol, 0)
	trades := make([]matching.TradeView, 0, len(all))
	for _, v := range all {
		if v.Market != "spot" {
			continue
		}
		if !v.MarginMatches(margin) {
			continue
		}
		trades = append(trades, v)
	}
	if limit > 0 && len(trades) > limit {
		trades = trades[:limit]
	}
	response.JSON(c, gin.H{"trades": trades})
}

// handleDepth 返回某交易对的订单簿深度（来自 cmd/matching）。
func (s *Server) handleDepth(c *gin.Context) {
	symbol := c.Query("symbol")
	bids, asks, ok := s.client.Depth(symbol)
	if !ok {
		response.Error(c, 400, 400, "unknown symbol or matching unavailable")
		return
	}
	response.JSON(c, gin.H{"bids": aggregate(bids), "asks": aggregate(asks)})
}

// handleWS 升级为现货行情 WebSocket，推送成交与深度。
func (s *Server) handleWS(c *gin.Context) {
	s.hub.Handle(c.Writer, c.Request)
}

// reconcile 比对 spot 预冻结记录与账本冻结余额，返回每「用户:资产」的偏差。
func (s *Server) reconcile() map[string]settlement.AssetAmount {
	s.freezeMu.Lock()
	expected := make(map[int64]map[string]settlement.AssetAmount) // uid -> asset -> 预期冻结额
	for _, rec := range s.openOrders {
		if !rec.frozenQuote.IsZero() {
			if expected[rec.user] == nil {
				expected[rec.user] = make(map[string]settlement.AssetAmount)
			}
			expected[rec.user][rec.quote] = expected[rec.user][rec.quote].Add(rec.frozenQuote)
		}
		if !rec.frozenBase.IsZero() {
			if expected[rec.user] == nil {
				expected[rec.user] = make(map[string]settlement.AssetAmount)
			}
			expected[rec.user][rec.base] = expected[rec.user][rec.base].Add(rec.frozenBase)
		}
	}
	s.freezeMu.Unlock()

	dev := make(map[string]settlement.AssetAmount)
	for uid, assets := range expected {
		for asset, exp := range assets {
			_, frozen, ok := s.ledgerSvc.Balance(uid, asset)
			if !ok {
				dev[fmt.Sprintf("%d:%s", uid, asset)] = exp
				continue
			}
			if diff := frozen.Sub(exp); !diff.IsZero() {
				dev[fmt.Sprintf("%d:%s", uid, asset)] = diff
			}
		}
	}
	return dev
}

// handleReconcile 暴露资金对账结果（仅管理员，见 RegisterRoutes 的 AdminGuard）。
func (s *Server) handleReconcile(c *gin.Context) {
	dev := s.reconcile()
	response.JSON(c, gin.H{"balanced": len(dev) == 0, "deviation": dev})
}

// Close 停止行情订阅。
func (s *Server) Close() {
	s.cancel()
}
