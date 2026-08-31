package matching

import (
	"context"
	"time"
)

// EventType 是 WAL 事件类型。
type EventType string

const (
	// EventSubmit 一笔订单被提交（含完整 Order）。
	EventSubmit EventType = "submit"
	// EventCancel 一笔订单被撤销（仅含 OrderID）。
	EventCancel EventType = "cancel"
)

// OrderEvent 是撮合引擎 WAL 的一条记录，用于崩溃恢复与多实例状态对齐。
// 提交订单必须在「写入 WAL」之后才应用到内存订单簿，保证恢复时可完整重放。
type OrderEvent struct {
	Seq     int64     `json:"seq"`            // 由 Store 分配，全局单调递增
	Symbol  string    `json:"symbol"`         // 交易对
	Type    EventType `json:"type"`           // submit / cancel
	Order   *Order    `json:"order,omitempty"` // submit 时携带完整订单
	OrderID int64     `json:"order_id,omitempty"` // cancel 时携带被撤订单 ID
	Ts      int64     `json:"ts"`             // 事件时间戳（unix 纳秒）
}

// Store 是撮合引擎的持久化与协调抽象。它同时承担四个职责：
//  1. 全局唯一且单调递增的订单号分配（解决多实例 order_id 冲突）；
//  2. WAL：在订单应用到内存簿之前落盘，崩溃后可重放；
//  3. 全量快照：定期序列化整个订单簿，加速恢复、削减 WAL；
//  4. Leader 选举：保证同一时刻只有一个实例在写共享订单簿（单写者副本模型）。
//
// 实现：
//   - MemStore：纯内存，单实例开发与单测使用（无持久性）；
//   - MySQLStore：以 MySQL 为共享后端，真正支持多实例部署与崩溃恢复。
//
// 所有方法在出错时返回 error；调用方（撮合引擎）对 WAL/快照失败采取 best-effort
// 记录日志但不阻断撮合，leader 选举失败则进入 standby 重试。
type Store interface {
	// NextOrderID 返回全局唯一、单调递增的订单号。
	NextOrderID(ctx context.Context) (int64, error)
	// SetMinOrderID 保证后续分配的订单号严格大于 id（恢复后对齐本地计数器，MySQL 实现为 no-op）。
	SetMinOrderID(ctx context.Context, id int64) error

	// Append 在订单应用到内存簿「之前」持久化一条事件，返回 error 表示写入失败。
	Append(ctx context.Context, ev OrderEvent) error
	// SaveSnapshot 保存序列化后的全量簿状态，version 为所覆盖的最大 WAL seq。
	SaveSnapshot(ctx context.Context, version int64, state []byte) error
	// LoadSnapshot 返回最近一次快照的 version 与状态；无快照时 version<0、state 为空。
	LoadSnapshot(ctx context.Context) (version int64, state []byte, err error)
	// Replay 返回 seq>afterVersion 的所有事件，按 seq 升序（用于快照之后的增量重放）。
	Replay(ctx context.Context, afterVersion int64) ([]OrderEvent, error)
	// MaxSeq 返回当前 WAL 的最大 seq（快照 version 取此值）。
	MaxSeq(ctx context.Context) (int64, error)
	// PruneWAL 删除 seq<=seq 的 WAL 记录（快照完成后调用，削减历史）。
	PruneWAL(ctx context.Context, seq int64) error

	// TryAcquireLeader 尝试成为 leader；成功返回 true（同一时刻仅一个实例成功）。
	TryAcquireLeader(ctx context.Context, node string, ttl time.Duration) (bool, error)
	// RenewLeader 续约（仅当前 holder 可成功）；返回 false 表示已失去 leadership。
	RenewLeader(ctx context.Context, node string, ttl time.Duration) (bool, error)
	// ReleaseLeader 主动放弃 leadership。
	ReleaseLeader(ctx context.Context, node string) error
	// IsLeader 报告 node 当前是否为有效 leader（未过期）。
	IsLeader(ctx context.Context, node string) (bool, error)

	// ---- 成交流水 / 订单登记持久化（重启后历史不丢，见 DEVELOPMENT_TASKS 项2）----

	// AppendTrade 持久化一笔成交（best-effort：调用方不依赖其成功，失败仅记日志）。
	// seq 由存储后端分配（MySQL 自增 / 内存计数），与引擎内 e.tradeSeq 对齐。
	AppendTrade(ctx context.Context, t PersistedTrade) error
	// LoadTrades 返回全部已持久化成交流水（按 Seq 升序）；无记录返回空切片。
	LoadTrades(ctx context.Context) ([]PersistedTrade, error)
	// UpsertOrder 持久化/覆盖一笔订单登记（幂等：同 orderID 覆盖最新的状态与成交量）。
	UpsertOrder(ctx context.Context, o PersistedOrder) error
	// LoadOrders 返回全部已持久化订单登记（无序；调用方自行按 ID 索引）。
	LoadOrders(ctx context.Context) ([]PersistedOrder, error)

	// Close 释放底层资源（如 *sql.DB）。
	Close() error
}

// PersistedTrade 是成交流水的可持久化视图，字段与引擎内部 tradeRecord 一一对应。
// 经 Store 持久化后，撮合引擎重启可经 LoadTrades 重建 e.trades / e.userTrades，
// 消除原「成交流水仅内存、重启即丢」的缺口（futuresapi /orders、/trades 代理此簿）。
type PersistedTrade struct {
	Seq       int64  `json:"seq"`
	Symbol    string `json:"symbol"`
	Market    string `json:"market"`
	IsMargin  bool   `json:"is_margin"`
	Leverage  float64 `json:"leverage"`
	Price     Fixed  `json:"price"`
	Qty       Fixed  `json:"qty"`
	TakerID   int64  `json:"taker_id"`
	MakerID   int64  `json:"maker_id"`
	TakerSide Side   `json:"taker_side"`
	TakerOID  int64  `json:"taker_oid"`
	MakerOID  int64  `json:"maker_oid"`
	Time      int64  `json:"time"`
}

// PersistedOrder 是订单登记表的可持久化视图，字段与引擎内部 orderMeta 一一对应。
// 用于重启后重建非挂单（filled/canceled/partial 已离场）订单的历史状态，
// 使 ListOrders 在崩溃恢复后仍返回完整订单生命周期。
type PersistedOrder struct {
	ID          int64       `json:"id"`
	UserID      int64       `json:"user_id"`
	Symbol      string      `json:"symbol"`
	Market      string      `json:"market"`
	IsMargin    bool        `json:"is_margin"`
	Leverage    float64     `json:"leverage"`
	Side        Side        `json:"side"`
	Price       Fixed       `json:"price"`
	Qty         Fixed       `json:"qty"`
	FilledQty   Fixed       `json:"filled_qty"`
	TimeInForce string      `json:"time_in_force"`
	Status      OrderStatus `json:"status"`
	CreatedAt   int64       `json:"created_at"`
	UpdatedAt   int64       `json:"updated_at"`
}
