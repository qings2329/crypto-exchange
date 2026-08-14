package adminapi

import (
	"sync"
	"time"
)

// 本文件定义管理后台的内存管理态（原型骨架）：用户/交易对/公链/币种/充值提币/通知，
// 以及风控、账本对账、服务健康的只读快照。数据启动 seed、CRUD 落在内存，重启即丢失；
// 生产应替换为 user/settlement/oracle 等真实下游（见 DEVELOPMENT_TASKS.md §19）。

// AdminUser 是用户与账户管理视图（聚合账户/余额/KYC/冻结状态）。
type AdminUser struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	Status    string    `json:"status"` // active | frozen
	KYC       string    `json:"kyc"`    // none | pending | verified
	Balance   float64   `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
}

// SymbolConfig 是交易对/参数配置（手续费率、杠杆上限、上下线）。
type SymbolConfig struct {
	Symbol        string  `json:"symbol"`
	Base          string  `json:"base"`
	Quote         string  `json:"quote"`
	Status        string  `json:"status"` // online | offline
	FeeRate       float64 `json:"fee_rate"`
	MaxLeverage   int     `json:"max_leverage"`
	MinQty        float64 `json:"min_qty"`
}

// Chain 是公链管理视图。
type Chain struct {
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	Symbol           string    `json:"symbol"`
	Confirmations   int       `json:"confirmations"`
	DepositEnabled   bool      `json:"deposit_enabled"`
	WithdrawEnabled  bool      `json:"withdraw_enabled"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Coin 是币种管理视图（归属公链、精度、提币手续费）。
type Coin struct {
	ID           int64     `json:"id"`
	Symbol       string    `json:"symbol"`
	Name         string    `json:"name"`
	Chain        string    `json:"chain"`
	Precision    int       `json:"precision"`
	WithdrawFee  float64   `json:"withdraw_fee"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Deposit 是充值记录（公链入账）。
type Deposit struct {
	ID       int64     `json:"id"`
	UserID   int64     `json:"user_id"`
	Coin     string    `json:"coin"`
	Chain    string    `json:"chain"`
	Amount   float64   `json:"amount"`
	TxHash   string    `json:"tx_hash"`
	Status   string    `json:"status"` // pending | confirmed
	Time     time.Time `json:"time"`
}

// Withdrawal 是提币记录（需审核）。
type Withdrawal struct {
	ID       int64     `json:"id"`
	UserID   int64     `json:"user_id"`
	Coin     string    `json:"coin"`
	Chain    string    `json:"chain"`
	Amount   float64   `json:"amount"`
	Address  string    `json:"address"`
	TxHash   string    `json:"tx_hash"`
	Status   string    `json:"status"` // pending | approved | rejected
	Time     time.Time `json:"time"`
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
	UserID    int64     `json:"user_id"`
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"`
	Size      float64   `json:"size"`
	LiqPrice  float64   `json:"liq_price"`
	Equity    float64   `json:"equity"`
	Detected  time.Time `json:"detected"`
}

// RiskSnapshot 是风控与强平监控的只读快照（实时取自 futures 服务）。
type RiskSnapshot struct {
	UpdatedAt      time.Time          `json:"updated_at"`
	Liquidations   []LiquidationItem `json:"liquidations"`
	InsuranceFund  float64            `json:"insurance_fund"`
	SocializedLoss float64            `json:"socialized_loss"`
	ADLQueue       []string           `json:"adl_queue"` // 自动化减仓排队（"uid:symbol"）
	// Notes 记录部分上游拉取失败的信息（降级时填充）。
	Notes string `json:"notes,omitempty"`
}

// LedgerSummary 是账本对账快照（实时取自 futures 服务的复式记账对账探针）。
type LedgerSummary struct {
	UpdatedAt      time.Time `json:"updated_at"`
	TotalAssets    float64   `json:"total_assets"`
	SettlementBal  float64   `json:"settlement_balance"`
	Reconciled     bool      `json:"reconciled"`
	Discrepancy    float64   `json:"discrepancy"`
	// Notes 记录部分上游拉取失败的信息（降级时填充）。
	Notes string `json:"notes,omitempty"`
}

// ServiceHealth 是单个微服务的健康状态（运营看板用）。
type ServiceHealth struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"` // up | down | degraded
	LatencyMs int64     `json:"latency_ms"`
	LastCheck time.Time `json:"last_check"`
}

// Store 是管理后台的内存管理态，所有写操作受锁保护。
type Store struct {
	mu sync.RWMutex

	users       []AdminUser
	symbols     []SymbolConfig
	chains      []Chain
	coins       []Coin
	deposits    []Deposit
	withdrawals []Withdrawal
	notifs      []Notification

	// 会话态：充值提币来自 futures 上游实时数据；下面两个映射仅用于在审批时把
	// 前端使用的稳定数字 id 反查回链上事件（TxHash），并缓存本次会话的审批结果。
	wdByID       map[int64]Withdrawal
	wdApprovals  map[int64]string // id -> approved|rejected

	risk   RiskSnapshot
	ledger LedgerSummary
	health []ServiceHealth

	seqUser int64
	seqSym  int64
	seqChain int64
	seqCoin int64
	seqDep  int64
	seqWd   int64
	seqNotif int64
}

// NewStore 构造并 seed 示例数据的管理态。
func NewStore() *Store {
	s := &Store{}
	now := time.Now()
	// 用户
	s.users = []AdminUser{
		{ID: 1001, Username: "alice", Email: "alice@x.com", Status: "active", KYC: "verified", Balance: 125000.5, CreatedAt: now.Add(-72 * time.Hour)},
		{ID: 1002, Username: "bob", Email: "bob@x.com", Status: "active", KYC: "pending", Balance: 3400.0, CreatedAt: now.Add(-48 * time.Hour)},
		{ID: 1003, Username: "carol", Email: "carol@x.com", Status: "frozen", KYC: "verified", Balance: 0, CreatedAt: now.Add(-24 * time.Hour)},
	}
	s.seqUser = 1003
	// 交易对
	s.symbols = []SymbolConfig{
		{Symbol: "BTC_USDT", Base: "BTC", Quote: "USDT", Status: "online", FeeRate: 0.001, MaxLeverage: 20, MinQty: 0.0001},
		{Symbol: "ETH_USDT", Base: "ETH", Quote: "USDT", Status: "online", FeeRate: 0.001, MaxLeverage: 20, MinQty: 0.001},
		{Symbol: "BTC_USDT_PERP", Base: "BTC", Quote: "USDT", Status: "online", FeeRate: 0.0005, MaxLeverage: 100, MinQty: 0.0001},
	}
	s.seqSym = 3
	// 公链
	s.chains = []Chain{
		{ID: 1, Name: "Bitcoin", Symbol: "BTC", Confirmations: 3, DepositEnabled: true, WithdrawEnabled: true, UpdatedAt: now},
		{ID: 2, Name: "Ethereum", Symbol: "ETH", Confirmations: 12, DepositEnabled: true, WithdrawEnabled: true, UpdatedAt: now},
		{ID: 3, Name: "Tron", Symbol: "TRX", Confirmations: 20, DepositEnabled: true, WithdrawEnabled: false, UpdatedAt: now},
	}
	s.seqChain = 3
	// 币种
	s.coins = []Coin{
		{ID: 1, Symbol: "BTC", Name: "Bitcoin", Chain: "Bitcoin", Precision: 8, WithdrawFee: 0.0005, UpdatedAt: now},
		{ID: 2, Symbol: "ETH", Name: "Ethereum", Chain: "Ethereum", Precision: 18, WithdrawFee: 0.01, UpdatedAt: now},
		{ID: 3, Symbol: "USDT", Name: "Tether", Chain: "Ethereum", Precision: 6, WithdrawFee: 1, UpdatedAt: now},
	}
	s.seqCoin = 3
	// 充值提币
	s.deposits = []Deposit{
		{ID: 1, UserID: 1001, Coin: "BTC", Chain: "Bitcoin", Amount: 0.5, TxHash: "0xabc...", Status: "confirmed", Time: now.Add(-5 * time.Hour)},
		{ID: 2, UserID: 1002, Coin: "USDT", Chain: "Ethereum", Amount: 1000, TxHash: "0xdef...", Status: "pending", Time: now.Add(-1 * time.Hour)},
	}
	s.seqDep = 2
	s.withdrawals = []Withdrawal{
		{ID: 1, UserID: 1001, Coin: "BTC", Chain: "Bitcoin", Amount: 0.1, Address: "bc1xyz...", TxHash: "", Status: "pending", Time: now.Add(-30 * time.Minute)},
	}
	s.seqWd = 1
	// 通知
	s.notifs = []Notification{
		{ID: 1, Title: "系统维护通知", Body: "今晚 02:00-02:30 进行撮合引擎升级。", Level: "info", CreatedAt: now.Add(-6 * time.Hour)},
		{ID: 2, Title: "ETH 提币临时关闭", Body: "Tron 链拥堵，USDT-TRC20 提币暂停。", Level: "warning", CreatedAt: now.Add(-2 * time.Hour)},
	}
	s.seqNotif = 2
	s.wdByID = map[int64]Withdrawal{}
	s.wdApprovals = map[int64]string{}
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
		UpdatedAt:      now,
		TotalAssets:    18_420_000,
		SettlementBal:  18_420_000,
		Reconciled:     true,
		Discrepancy:    0,
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
