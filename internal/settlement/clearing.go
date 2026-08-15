// 交易清算层：消费撮合引擎发布的成交流（exchange.trades），把每笔成交的手续费/清算
// 入账到交易所自有结算账户，形成"撮合成交 -> Kafka -> 清算入账"的资金闭环（见 T-02 / T-16）。
//
// 与 internal/settlement 的"链上清结算"（DepositGateway/WithdrawGateway，依赖链上 RPC）不同，
// 本层处理的是**交易手续费清算**：现货/合约的下单与用户余额变动由 spot/futures 直接经
// Ledger 处理，而交易所应收取的手续费则由独立的清算服务统一归集、对账，避免与用户资金混算。
//
// 幂等：Kafka 为 at-least-once 语义，同一成交可能重复投递。以「交易字段确定性哈希」作为
// 幂等键（INSERT IGNORE / 内存去重），重复消息安全跳过，仅首次落库计入统计。

package settlement

import (
	"hash/fnv"
	"strconv"
	"sync"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
)

// DefaultTradeFeeRate 是交易所对 taker（主动成交方）收取的默认手续费率（0.1%）。
// 生产可按用户等级/交易对维度细化；此处以全局统一费率起步，可由配置覆盖。
const DefaultTradeFeeRate = 0.001

// ClearedTrade 是一笔已清算入账的成交（清算服务的核心流水）。
type ClearedTrade struct {
	ID        int64     `json:"id"`          // 成交幂等键（FNV64 of 交易字段）
	Symbol    string    `json:"symbol"`
	Price     float64   `json:"price"`
	Qty       float64   `json:"qty"`
	TakerID   int64     `json:"taker_id"`
	MakerID   int64     `json:"maker_id"`
	TakerSide string    `json:"taker_side"` // buy/sell
	// Fee 是交易所就该笔成交收取的手续费（taker 侧计费）：Fee = Price * Qty * TradeFeeRate。
	Fee float64 `json:"fee"`
	// Ts 是撮合成交的 unix 毫秒（来自 TradeEvent）。
	Ts int64 `json:"ts"`
	// ClearedAt 是清算服务入账时间（用于排序/对账）。
	ClearedAt time.Time `json:"cleared_at"`
}

// ClearingStats 是清算聚合统计（内存实时视图，供 /stats 端点）。
type ClearingStats struct {
	TotalTrades     int64              `json:"total_trades"`
	TotalVolume     float64            `json:"total_volume"`     // Σ Price*Qty
	TotalCommission float64            `json:"total_commission"` // Σ Fee
	BySymbol        map[string]float64 `json:"by_symbol"`        // symbol -> 累计手续费
}

// ClearingStore 清算流水的持久化抽象（MySQL 优先，失败回退内存）。
type ClearingStore interface {
	// Record 写入一笔清算成交；返回 inserted=true 表示首次写入、false 表示重复（幂等跳过）。
	Record(t ClearedTrade) (inserted bool, err error)
	// Recent 按入账时间倒序返回最近 limit 笔清算成交（limit<=0 取默认）。
	Recent(limit int) ([]ClearedTrade, error)
	// Close 释放底层资源。
	Close() error
}

// Clearer 是交易清算处理器：把 mq.TradeEvent 转换为 ClearedTrade 并入账、聚合统计。
// 并发安全（Kafka 消费组回调可能在多协程触发）。
type Clearer struct {
	mu      sync.Mutex
	store   ClearingStore
	feeRate float64
	stats   ClearingStats
}

// NewClearer 构造清算处理器；feeRate<=0 时采用 DefaultTradeFeeRate。
func NewClearer(store ClearingStore, feeRate float64) *Clearer {
	if feeRate <= 0 {
		feeRate = DefaultTradeFeeRate
	}
	return &Clearer{
		store:   store,
		feeRate: feeRate,
		stats:   ClearingStats{BySymbol: map[string]float64{}},
	}
}

// FeeRate 返回当前生效的手续费率。
func (cl *Clearer) FeeRate() float64 { return cl.feeRate }

// Clear 处理一笔成交流：计算手续费、幂等入账、更新聚合统计。幂等由 store 层保证，
// 重复消息不计入统计、不影响既有数据。
func (cl *Clearer) Clear(ev mq.TradeEvent) error {
	t := ClearedTrade{
		ID:        TradeID(ev),
		Symbol:    ev.Symbol,
		Price:     ev.Price,
		Qty:       ev.Qty,
		TakerID:   ev.TakerID,
		MakerID:   ev.MakerID,
		TakerSide: ev.TakerSide,
		Fee:       ev.Price * ev.Qty * cl.feeRate,
		Ts:        ev.Ts,
		ClearedAt: time.Now(),
	}
	inserted, err := cl.store.Record(t)
	if err != nil {
		return err
	}
	if !inserted {
		return nil
	}
	cl.mu.Lock()
	cl.stats.TotalTrades++
	cl.stats.TotalVolume += ev.Price * ev.Qty
	cl.stats.TotalCommission += t.Fee
	cl.stats.BySymbol[ev.Symbol] += t.Fee
	cl.mu.Unlock()
	return nil
}

// Stats 返回当前聚合统计快照。
func (cl *Clearer) Stats() ClearingStats {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	by := make(map[string]float64, len(cl.stats.BySymbol))
	for k, v := range cl.stats.BySymbol {
		by[k] = v
	}
	return ClearingStats{
		TotalTrades:     cl.stats.TotalTrades,
		TotalVolume:     cl.stats.TotalVolume,
		TotalCommission: cl.stats.TotalCommission,
		BySymbol:        by,
	}
}

// Recent 返回最近 limit 笔清算成交（透传 store）。
func (cl *Clearer) Recent(limit int) ([]ClearedTrade, error) {
	return cl.store.Recent(limit)
}

// TradeID 由成交关键字段计算确定性幂等键（FNV-64a）。相同成交字段必得相同 ID，
// 从而跨重启/跨投递去重。
func TradeID(ev mq.TradeEvent) int64 {
	h := fnv.New64a()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	// 固定精度序列化浮点，避免字符串化差异导致幂等键漂移。
	write(ev.Symbol)
	write(strconv.FormatFloat(ev.Price, 'f', -1, 64))
	write(strconv.FormatFloat(ev.Qty, 'f', -1, 64))
	write(strconv.FormatInt(ev.TakerID, 10))
	write(strconv.FormatInt(ev.MakerID, 10))
	write(ev.TakerSide)
	write(strconv.FormatInt(ev.Ts, 10))
	return int64(h.Sum64())
}
