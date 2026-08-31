package adminapi

import (
	"sync"
	"time"
)

// 本文件定义管理后台的内存管理态（原型骨架）：用户/充值提币（来自 futures 上游或示例
// 降级）/风控/账本对账/服务健康的只读快照与审批会话态。数据启动 seed、重启即丢失。
// 管理员自有配置（交易对/公链/币种/本地通知）已迁至 store_catalog.go 的 CatalogStore
// （MySQL 优先，失败回退内存），见 DEVELOPMENT_TASKS.md §19。

// AdminUser 是用户与账户管理视图（聚合账户/余额/KYC/冻结状态）。
type AdminUser struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	Status    string    `json:"status"` // active | frozen
	KYC       string    `json:"kyc"`    // none | pending | verified
	Level     int       `json:"level"`  // 用户等级：0=普通，1~5=VIP1~VIP5
	Balance   float64   `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
}

// SymbolConfig 是交易对/参数配置（手续费率、杠杆上限、上下线）。
type SymbolConfig struct {
	Symbol      string  `json:"symbol"`
	Base        string  `json:"base"`
	Quote       string  `json:"quote"`
	Status      string  `json:"status"` // online | offline
	FeeRate     float64 `json:"fee_rate"`
	MaxLeverage int     `json:"max_leverage"`
	MinQty      float64 `json:"min_qty"`
}

// Chain 是公链管理视图。
type Chain struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Symbol          string    `json:"symbol"`
	Confirmations   int       `json:"confirmations"`
	DepositEnabled  bool      `json:"deposit_enabled"`
	WithdrawEnabled bool      `json:"withdraw_enabled"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Coin 是币种管理视图（归属公链、精度、提币手续费）。
type Coin struct {
	ID          int64     `json:"id"`
	Symbol      string    `json:"symbol"`
	Name        string    `json:"name"`
	Chain       string    `json:"chain"`
	Precision   int       `json:"precision"`
	WithdrawFee float64   `json:"withdraw_fee"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Deposit 是充值记录（公链入账）。
type Deposit struct {
	ID     string    `json:"id"` // 真实链上标识（tx_hash），作为前端审批/对账锚点
	UserID int64     `json:"user_id"`
	Coin   string    `json:"coin"`
	Chain  string    `json:"chain"`
	Amount float64   `json:"amount"`
	TxHash string    `json:"tx_hash"`
	Status string    `json:"status"` // pending | confirmed
	Time   time.Time `json:"time"`
}

// DepositAddress 是用户充值地址视图（按 userID + chain 确定性派生，无持久化）。
type DepositAddress struct {
	UserID  int64  `json:"user_id"`
	Chain   string `json:"chain"`
	Address string `json:"address"`
}

// Withdrawal 是提币记录（需审核）。
type Withdrawal struct {
	ID      string    `json:"id"` // 真实 futures hold_id（字符串），审批路由直接以此为锚点
	UserID  int64     `json:"user_id"`
	Coin    string    `json:"coin"`
	Chain   string    `json:"chain"`
	Amount  float64   `json:"amount"`
	Address string    `json:"address"`
	TxHash  string    `json:"tx_hash"`
	Status  string    `json:"status"` // pending | approved | rejected
	Time    time.Time `json:"time"`
}

// Notification 是运营通知（live=来自通知服务实时数据；local=管理后台本地公告）。
type Notification struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Level     string    `json:"level"` // info | warning | critical
	CreatedAt time.Time `json:"created_at"`
	// Source 标识来源：live（通知服务实时推送）或 local（管理后台本地公告）。
	Source string `json:"source,omitempty"`
}

// LiquidationItem 是强平队列中的一条待强平持仓。
type LiquidationItem struct {
	UserID   int64     `json:"user_id"`
	Symbol   string    `json:"symbol"`
	Side     string    `json:"side"`
	Size     float64   `json:"size"`
	LiqPrice float64   `json:"liq_price"`
	Equity   float64   `json:"equity"`
	Detected time.Time `json:"detected"`
}

// RiskSnapshot 是风控与强平监控的只读快照（实时取自 futures 服务）。
type RiskSnapshot struct {
	UpdatedAt      time.Time         `json:"updated_at"`
	Liquidations   []LiquidationItem `json:"liquidations"`
	InsuranceFund  float64           `json:"insurance_fund"`
	SocializedLoss float64           `json:"socialized_loss"`
	ADLQueue       []string          `json:"adl_queue"` // 自动化减仓排队（"uid:symbol"）
	// Notes 记录部分上游拉取失败的信息（降级时填充）。
	Notes string `json:"notes,omitempty"`
}

// LedgerSummary 是账本对账快照（实时取自 futures 服务的复式记账对账探针 + settlement 清算聚合）。
type LedgerSummary struct {
	UpdatedAt     time.Time         `json:"updated_at"`
	TotalAssets   float64           `json:"total_assets"`
	SettlementBal float64           `json:"settlement_balance"`
	Reconciled    bool              `json:"reconciled"`
	Discrepancy   float64           `json:"discrepancy"`
	Settlement    SettlementSummary `json:"settlement"`
	// Assets 是按币种拆分的平台链上库存总量（来自 futures wallet inventory 的 onchain_total）。
	// 用于平台资产分布统计；不进入任何资金移动路径。
	Assets []AssetTotal `json:"assets"`
	// Notes 记录部分上游拉取失败的信息（降级时填充）。
	Notes string `json:"notes,omitempty"`
}

// AssetTotal 是单币种平台链上库存总量（来自 futures wallet inventory 的 onchain_total）。
type AssetTotal struct {
	Asset        string  `json:"asset"`
	OnchainTotal float64 `json:"onchain_total"`
}

// SettlementSummary 是结算服务的清算聚合快照（实时取自 settlement 服务的 /stats 与 /cleared）。
type SettlementSummary struct {
	Enabled        bool               `json:"enabled"`
	TotalTrades    int64              `json:"total_trades"`
	TotalVolume    float64            `json:"total_volume"`
	TotalCommission float64           `json:"total_commission"`
	BySymbol       map[string]float64 `json:"by_symbol"`
	Recent         []ClearedTradeView `json:"recent"`
	Notes          string             `json:"notes,omitempty"`
}

// ClearedTradeView 是最近清算成交的精简视图（供运营看板展示）。
type ClearedTradeView struct {
	ID        int64   `json:"id"` // 与 settlement 服务一致，为 FNV64 整型幂等键。
	Symbol    string  `json:"symbol"`
	Price     float64 `json:"price"`
	Qty       float64 `json:"qty"`
	TakerID   int64   `json:"taker_id"`
	MakerID   int64   `json:"maker_id"`
	TakerSide string  `json:"taker_side"`
	Fee       float64 `json:"fee"`
	Ts        int64   `json:"ts"`
}

// ServiceHealth 是单个微服务的健康状态（运营看板用）。
type ServiceHealth struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"` // up | down | degraded
	LatencyMs int64     `json:"latency_ms"`
	LastCheck time.Time `json:"last_check"`
}

// Store 是管理后台的内存管理态，所有写操作受锁保护。
// 注意：交易对/公链/币种/本地通知等管理员自有配置已迁至 CatalogStore（见 store_catalog.go），
// 本 Store 仅保留风控/账本/健康等只读快照、以及提现审批会话态。用户与充值提币均来自上游
// 实时数据，不在此 seed 任何示例（上游不可达时列表返回空数组而非伪造记录，见 handlers.go）。
type Store struct {
	mu sync.RWMutex

	// 会话态：充值提币来自 futures 上游实时数据。审批锚点直接采用 futures 返回的
	// 真实 hold_id（字符串），不再经 stableID 哈希与服务端可变 map 反查，杜绝哈希碰撞/TOCTOU 导致的错审。
	wdByID      map[string]Withdrawal // hold_id -> 记录（仅回显缓存，审批不依赖它）
	wdApprovals map[string]string     // hold_id -> approved|rejected（本会话审批结果回显 + 终态短路）

	risk   RiskSnapshot
	ledger LedgerSummary
	health []ServiceHealth
}

// NewStore 构造管理态（无示例 seed：用户与充值提币均来自上游实时数据，
// 上游不可达时列表返回空数组而非伪造记录，见 handlers.go）。
func NewStore() *Store {
	s := &Store{}
	now := time.Now()
	// 注意：不再 seed 伪造的充值/提现/用户示例数据（发现 4 及其对称项）。上游不可达时，
	// listDeposits/listWithdrawals/listUsers 返回 degraded 空列表，避免向运营展示不存在的资金/账户记录。
	s.wdByID = map[string]Withdrawal{}
	s.wdApprovals = map[string]string{}
	// 风控快照
	s.risk = RiskSnapshot{
		UpdatedAt:      now,
		Liquidations:   []LiquidationItem{},
		InsuranceFund:  2_350_000,
		SocializedLoss: 0,
		ADLQueue:       []string{},
	}
	// 账本对账
	s.ledger = LedgerSummary{
		UpdatedAt:     now,
		TotalAssets:   18_420_000,
		SettlementBal: 18_420_000,
		Reconciled:    true,
		Discrepancy:   0,
	}
	// 服务健康
	s.health = []ServiceHealth{
		{Name: "spot", Status: "up", LatencyMs: 3, LastCheck: now},
		{Name: "futures", Status: "up", LatencyMs: 4, LastCheck: now},
		{Name: "matching", Status: "up", LatencyMs: 2, LastCheck: now},
		{Name: "notification", Status: "degraded", LatencyMs: 21, LastCheck: now},
	}
	return s
}
