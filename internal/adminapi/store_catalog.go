package adminapi

import (
	"errors"
	"sync"
	"time"
)

// 本文件定义管理后台"交易对/公链/币种/本地通知"等自有配置（Catalog）的持久化抽象，
// 与 store_adminaccount.go 的 AdminStore 平行：含内存实现（无 MySQL 时回退）、
// MySQL 工厂与首次启动的引导种子。这些配置此前仅存于内存（见 DEVELOPMENT_TASKS.md §19），
// 重启即丢失；本文件使其可持久化。

// ErrCatalogNotFound 表示配置实体不存在。
var ErrCatalogNotFound = errors.New("catalog entity not found")

// ErrCatalogInvalid 表示配置更新违反约束（如启用充提但 RPC 端点为空）。
var ErrCatalogInvalid = errors.New("invalid catalog update")

// CatalogStore 抽象交易对/公链/币种/本地通知的持久化（管理员自有配置）。
type CatalogStore interface {
	// 交易对
	ListSymbols() ([]SymbolConfig, error)
	UpsertSymbol(sym SymbolConfig) (SymbolConfig, error) // 按 symbol 更新或插入

	// 公链
	ListChains() ([]Chain, error)
	CreateChain(ch Chain) (Chain, error)              // 分配 ID + UpdatedAt
	UpdateChain(id int64, patch Chain) (Chain, error) // 部分更新；不存在返回 ErrCatalogNotFound

	// 币种
	ListCoins() ([]Coin, error)
	CreateCoin(coin Coin) (Coin, error)
	UpdateCoin(id int64, patch Coin) (Coin, error)

	// 本地通知（管理后台公告，区别于 notification 服务实时推送）
	ListNotifications() ([]Notification, error)
	CreateNotification(n Notification) (Notification, error) // 分配 ID + CreatedAt，Source="local"
	DeleteNotification(id int64) error                       // 不存在返回 ErrCatalogNotFound
}

// ---- 内存实现（无 MySQL 时回退，且为默认本地运行态） ----

type memCatalogStore struct {
	mu sync.RWMutex

	symbols []SymbolConfig
	chains  []Chain
	coins   []Coin
	notifs  []Notification

	seqSym   int64
	seqChain int64
	seqCoin  int64
	seqNotif int64
}

// NewMemCatalogStore 构造内存实现（启动 seed 由 SeedCatalog 负责）。
func NewMemCatalogStore() CatalogStore {
	return &memCatalogStore{}
}

func (s *memCatalogStore) ListSymbols() ([]SymbolConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SymbolConfig, len(s.symbols))
	copy(out, s.symbols)
	return out, nil
}

func (s *memCatalogStore) UpsertSymbol(sym SymbolConfig) (SymbolConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.symbols {
		if s.symbols[i].Symbol == sym.Symbol {
			s.symbols[i] = sym
			return s.symbols[i], nil
		}
	}
	s.seqSym++
	s.symbols = append(s.symbols, sym)
	return sym, nil
}

func (s *memCatalogStore) ListChains() ([]Chain, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Chain, len(s.chains))
	copy(out, s.chains)
	return out, nil
}

func (s *memCatalogStore) CreateChain(ch Chain) (Chain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqChain++
	ch.ID = s.seqChain
	ch.UpdatedAt = time.Now()
	s.chains = append(s.chains, ch)
	return ch, nil
}

// UpdateChain 部分更新：字符串仅当非空、Confirmations 仅当非 0 才覆盖；布尔按 patch 值覆盖。
// 先按现有值 + patch 计算最终值并做组合校验，校验通过再写入，避免拒绝更新时
// 内存已被部分修改导致状态不一致。
func (s *memCatalogStore) UpdateChain(id int64, patch Chain) (Chain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.chains {
		if s.chains[i].ID == id {
			finalRpc := s.chains[i].RpcEndpoint
			if patch.RpcEndpoint != "" {
				finalRpc = patch.RpcEndpoint
			}
			if (patch.DepositEnabled || patch.WithdrawEnabled) && finalRpc == "" {
				return Chain{}, ErrCatalogInvalid
			}
			if patch.Name != "" {
				s.chains[i].Name = patch.Name
			}
			if patch.Symbol != "" {
				s.chains[i].Symbol = patch.Symbol
			}
			if patch.Confirmations != 0 {
				s.chains[i].Confirmations = patch.Confirmations
			}
			if patch.RpcEndpoint != "" {
				s.chains[i].RpcEndpoint = patch.RpcEndpoint
			}
			s.chains[i].DepositEnabled = patch.DepositEnabled
			s.chains[i].WithdrawEnabled = patch.WithdrawEnabled
			s.chains[i].UpdatedAt = time.Now()
			return s.chains[i], nil
		}
	}
	return Chain{}, ErrCatalogNotFound
}

func (s *memCatalogStore) ListCoins() ([]Coin, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Coin, len(s.coins))
	copy(out, s.coins)
	return out, nil
}

func (s *memCatalogStore) CreateCoin(coin Coin) (Coin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqCoin++
	coin.ID = s.seqCoin
	coin.UpdatedAt = time.Now()
	s.coins = append(s.coins, coin)
	return coin, nil
}

// UpdateCoin 部分更新：字符串仅当非空、Precision/WithdrawFee 仅当非 0 才覆盖。
func (s *memCatalogStore) UpdateCoin(id int64, patch Coin) (Coin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.coins {
		if s.coins[i].ID == id {
			if patch.Symbol != "" {
				s.coins[i].Symbol = patch.Symbol
			}
			if patch.Name != "" {
				s.coins[i].Name = patch.Name
			}
			if patch.Chain != "" {
				s.coins[i].Chain = patch.Chain
			}
			if patch.Precision != 0 {
				s.coins[i].Precision = patch.Precision
			}
			if patch.WithdrawFee != 0 {
				s.coins[i].WithdrawFee = patch.WithdrawFee
			}
			s.coins[i].UpdatedAt = time.Now()
			return s.coins[i], nil
		}
	}
	return Coin{}, ErrCatalogNotFound
}

func (s *memCatalogStore) ListNotifications() ([]Notification, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Notification, len(s.notifs))
	copy(out, s.notifs)
	return out, nil
}

func (s *memCatalogStore) CreateNotification(n Notification) (Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqNotif++
	n.ID = s.seqNotif
	n.CreatedAt = time.Now()
	n.Source = "local"
	s.notifs = append(s.notifs, n)
	return n, nil
}

func (s *memCatalogStore) DeleteNotification(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.notifs {
		if s.notifs[i].ID == id {
			s.notifs = append(s.notifs[:i], s.notifs[i+1:]...)
			return nil
		}
	}
	return ErrCatalogNotFound
}

// NewCatalogStore 优先返回 MySQL 实现；若 DSN 为空或连接/迁移失败，则回退内存实现
// （isMem=true）。本地无 MySQL 时可正常运行以验证逻辑。
func NewCatalogStore(dsn string) (store CatalogStore, isMem bool, err error) {
	if dsn == "" {
		return NewMemCatalogStore(), true, nil
	}
	ms, e := NewMySQLCatalogStore(dsn)
	if e != nil {
		return NewMemCatalogStore(), true, e
	}
	return ms, false, nil
}

// SeedCatalog 在各配置表为空时写入示例数据（交易对/公链/币种/通知），
// 已有数据则跳过。保证 MySQL 与内存实现都能拿到一致的演示数据。
func SeedCatalog(store CatalogStore) error {
	if syms, _ := store.ListSymbols(); len(syms) == 0 {
		for _, sym := range []SymbolConfig{
			{Symbol: "BTC_USDT", Base: "BTC", Quote: "USDT", Status: "online", FeeRate: 0.001, MaxLeverage: 20, MinQty: 0.0001},
			{Symbol: "ETH_USDT", Base: "ETH", Quote: "USDT", Status: "online", FeeRate: 0.001, MaxLeverage: 20, MinQty: 0.001},
			{Symbol: "BTC_USDT_PERP", Base: "BTC", Quote: "USDT", Status: "online", FeeRate: 0.0005, MaxLeverage: 100, MinQty: 0.0001},
		} {
			if _, err := store.UpsertSymbol(sym); err != nil {
				return err
			}
		}
	}
	if chs, _ := store.ListChains(); len(chs) == 0 {
		now := time.Now()
		for _, ch := range []Chain{
			{Name: "Bitcoin", Symbol: "BTC", Confirmations: 3, DepositEnabled: true, WithdrawEnabled: true, RpcEndpoint: "", UpdatedAt: now},
			{Name: "Ethereum", Symbol: "ETH", Confirmations: 12, DepositEnabled: true, WithdrawEnabled: true, RpcEndpoint: "", UpdatedAt: now},
			{Name: "Tron", Symbol: "TRX", Confirmations: 20, DepositEnabled: true, WithdrawEnabled: false, RpcEndpoint: "", UpdatedAt: now},
			{Name: "Litecoin", Symbol: "LTC", Confirmations: 6, DepositEnabled: true, WithdrawEnabled: true, RpcEndpoint: "", UpdatedAt: now},
			{Name: "Dogecoin", Symbol: "DOGE", Confirmations: 6, DepositEnabled: true, WithdrawEnabled: true, RpcEndpoint: "", UpdatedAt: now},
		} {
			if _, err := store.CreateChain(ch); err != nil {
				return err
			}
		}
	}
	if coins, _ := store.ListCoins(); len(coins) == 0 {
		now := time.Now()
		for _, c := range []Coin{
			{Symbol: "BTC", Name: "Bitcoin", Chain: "Bitcoin", Precision: 8, WithdrawFee: 0.0005, UpdatedAt: now},
			{Symbol: "ETH", Name: "Ethereum", Chain: "Ethereum", Precision: 18, WithdrawFee: 0.01, UpdatedAt: now},
			{Symbol: "USDT", Name: "Tether", Chain: "Ethereum", Precision: 6, WithdrawFee: 1, UpdatedAt: now},
			{Symbol: "LTC", Name: "Litecoin", Chain: "Litecoin", Precision: 8, WithdrawFee: 0.001, UpdatedAt: now},
			{Symbol: "DOGE", Name: "Dogecoin", Chain: "Dogecoin", Precision: 8, WithdrawFee: 1.0, UpdatedAt: now},
		} {
			if _, err := store.CreateCoin(c); err != nil {
				return err
			}
		}
	}
	if ns, _ := store.ListNotifications(); len(ns) == 0 {
		now := time.Now()
		for _, n := range []Notification{
			{Title: "系统维护通知", Body: "今晚 02:00-02:30 进行撮合引擎升级。", Level: "info", CreatedAt: now},
			{Title: "ETH 提币临时关闭", Body: "Tron 链拥堵，USDT-TRC20 提币暂停。", Level: "warning", CreatedAt: now},
		} {
			if _, err := store.CreateNotification(n); err != nil {
				return err
			}
		}
	}
	return nil
}
