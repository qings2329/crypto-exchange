package adminapi

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// 管理配置（Catalog）模块迁移版本号。
// 注意：必须错开 user 模块的 9101-9113（含其 DSN 默认共享同一 ce_schema_migrations 版本表），
// 否则 user 先落版本会导致 catalog 建表迁移被跳过、回退内存存储。故使用 9301-9304。
const (
	catalogMigVerSymbols       = 9301
	catalogMigVerChains        = 9302
	catalogMigVerCoins         = 9303
	catalogMigVerNotifications = 9304
	catalogMigVerRPCEndpoint   = 9305
)

// CatalogMigrations 是 Catalog 模块的建表迁移，运行时由 NewMySQLCatalogStore 应用。
var CatalogMigrations = []migrate.Migration{
	{
		Version: catalogMigVerSymbols,
		Name:    "create_ce_admin_symbols",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_symbols (
    symbol       VARCHAR(32)  NOT NULL,
    base         VARCHAR(16)  NOT NULL DEFAULT '',
    quote        VARCHAR(16)  NOT NULL DEFAULT '',
    status       VARCHAR(16)  NOT NULL DEFAULT 'online',
    fee_rate     DOUBLE       NOT NULL DEFAULT 0,
    max_leverage INT          NOT NULL DEFAULT 0,
    min_qty      DOUBLE       NOT NULL DEFAULT 0,
    PRIMARY KEY (symbol)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_symbols;",
	},
	{
		Version: catalogMigVerChains,
		Name:    "create_ce_admin_chains",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_chains (
    id              BIGINT      NOT NULL AUTO_INCREMENT,
    name            VARCHAR(64) NOT NULL DEFAULT '',
    symbol          VARCHAR(16) NOT NULL DEFAULT '',
    confirmations   INT         NOT NULL DEFAULT 0,
    deposit_enabled TINYINT     NOT NULL DEFAULT 1,
    withdraw_enabled TINYINT    NOT NULL DEFAULT 0,
    updated_at      DATETIME(3) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_chains;",
	},
	{
		// RPC 端点入表：ce_admin_chains 增加 rpc_endpoint 列，作为链清结算层
		// 连接各公链节点的单一数据源（替代散落的 CHAIN_RPC_ENDPOINT_* 环境变量）。
		Version: catalogMigVerRPCEndpoint,
		Name:    "alter_ce_admin_chains_rpc_endpoint",
		Up:      "ALTER TABLE ce_admin_chains ADD COLUMN rpc_endpoint VARCHAR(512) NOT NULL DEFAULT ''",
		Down:    "ALTER TABLE ce_admin_chains DROP COLUMN rpc_endpoint",
	},
	{
		Version: catalogMigVerCoins,
		Name:    "create_ce_admin_coins",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_coins (
    id           BIGINT      NOT NULL AUTO_INCREMENT,
    symbol       VARCHAR(16) NOT NULL DEFAULT '',
    name         VARCHAR(64) NOT NULL DEFAULT '',
    chain        VARCHAR(32) NOT NULL DEFAULT '',
    decimals     INT         NOT NULL DEFAULT 0,
    withdraw_fee DOUBLE      NOT NULL DEFAULT 0,
    updated_at   DATETIME(3) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_coins;",
	},
	{
		Version: catalogMigVerNotifications,
		Name:    "create_ce_admin_notifications",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_notifications (
    id         BIGINT      NOT NULL AUTO_INCREMENT,
    title      VARCHAR(255) NOT NULL DEFAULT '',
    body       TEXT,
    level      VARCHAR(16) NOT NULL DEFAULT 'info',
    created_at DATETIME(3) NOT NULL,
    source     VARCHAR(16) NOT NULL DEFAULT 'local',
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_notifications;",
	},
}

type mysqlCatalogStore struct {
	db *sql.DB
}

// NewMySQLCatalogStore 以 DSN 打开连接并应用迁移，返回 MySQL 实现。
func NewMySQLCatalogStore(dsn string) (CatalogStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := migrate.New(db, CatalogMigrations).Up(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("catalog migrations: %w", err)
	}
	// 探测关键表是否真实存在（防御迁移版本记录与物理表不一致的脏状态）。
	if _, err := db.Exec("SELECT 1 FROM ce_admin_symbols LIMIT 1"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("catalog tables not ready: %w", err)
	}
	return &mysqlCatalogStore{db: db}, nil
}

// ---- 交易对 ----

func (s *mysqlCatalogStore) ListSymbols() ([]SymbolConfig, error) {
	rows, err := s.db.Query(`SELECT symbol, base, quote, status, fee_rate, max_leverage, min_qty
		FROM ce_admin_symbols ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SymbolConfig{}
	for rows.Next() {
		var sym SymbolConfig
		if err := rows.Scan(&sym.Symbol, &sym.Base, &sym.Quote, &sym.Status, &sym.FeeRate, &sym.MaxLeverage, &sym.MinQty); err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

func (s *mysqlCatalogStore) UpsertSymbol(sym SymbolConfig) (SymbolConfig, error) {
	_, err := s.db.Exec(
		`INSERT INTO ce_admin_symbols (symbol, base, quote, status, fee_rate, max_leverage, min_qty)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE base=VALUES(base), quote=VALUES(quote), status=VALUES(status),
		   fee_rate=VALUES(fee_rate), max_leverage=VALUES(max_leverage), min_qty=VALUES(min_qty)`,
		sym.Symbol, sym.Base, sym.Quote, sym.Status, sym.FeeRate, sym.MaxLeverage, sym.MinQty)
	if err != nil {
		return SymbolConfig{}, err
	}
	return sym, nil
}

// ---- 公链 ----

func (s *mysqlCatalogStore) ListChains() ([]Chain, error) {
	rows, err := s.db.Query(`SELECT id, name, symbol, confirmations, deposit_enabled, withdraw_enabled, rpc_endpoint, updated_at
		FROM ce_admin_chains ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chain{}
	for rows.Next() {
		var ch Chain
		var dep, wd int
		var rpc string
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Symbol, &ch.Confirmations, &dep, &wd, &rpc, &ch.UpdatedAt); err != nil {
			return nil, err
		}
		ch.DepositEnabled = dep == 1
		ch.WithdrawEnabled = wd == 1
		ch.RpcEndpoint = rpc
		out = append(out, ch)
	}
	return out, rows.Err()
}

func (s *mysqlCatalogStore) CreateChain(ch Chain) (Chain, error) {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_admin_chains (name, symbol, confirmations, deposit_enabled, withdraw_enabled, rpc_endpoint, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ch.Name, ch.Symbol, ch.Confirmations, boolToInt(ch.DepositEnabled), boolToInt(ch.WithdrawEnabled), ch.RpcEndpoint, now)
	if err != nil {
		return Chain{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Chain{}, err
	}
	ch.ID = id
	ch.UpdatedAt = now
	return ch, nil
}

func (s *mysqlCatalogStore) GetChain(id int64) (Chain, error) {
	var ch Chain
	var dep, wd int
	err := s.db.QueryRow(
		`SELECT id, name, symbol, confirmations, deposit_enabled, withdraw_enabled, rpc_endpoint, updated_at
		 FROM ce_admin_chains WHERE id = ?`, id).
		Scan(&ch.ID, &ch.Name, &ch.Symbol, &ch.Confirmations, &dep, &wd, &ch.RpcEndpoint, &ch.UpdatedAt)
	if err == sql.ErrNoRows {
		return Chain{}, ErrCatalogNotFound
	}
	if err != nil {
		return Chain{}, err
	}
	ch.DepositEnabled = dep == 1
	ch.WithdrawEnabled = wd == 1
	return ch, nil
}

// UpdateChain 部分更新：字符串仅当非空、Confirmations 仅当非 0 才覆盖；布尔按 patch 值覆盖。
func (s *mysqlCatalogStore) UpdateChain(id int64, patch Chain) (Chain, error) {
	cur, err := s.GetChain(id)
	if err != nil {
		return Chain{}, err
	}
	// 先按现有值 + patch 计算最终值并做组合校验，校验通过再写入，
	// 避免拒绝更新时数据库已被部分修改。
	finalRpc := cur.RpcEndpoint
	if patch.RpcEndpoint != "" {
		finalRpc = patch.RpcEndpoint
	}
	if (patch.DepositEnabled || patch.WithdrawEnabled) && finalRpc == "" {
		return Chain{}, ErrCatalogInvalid
	}
	sets := []string{"deposit_enabled=?", "withdraw_enabled=?", "updated_at=NOW(3)"}
	args := []interface{}{boolToInt(patch.DepositEnabled), boolToInt(patch.WithdrawEnabled)}
	if patch.Name != "" {
		sets = append(sets, "name=?")
		args = append(args, patch.Name)
	}
	if patch.Symbol != "" {
		sets = append(sets, "symbol=?")
		args = append(args, patch.Symbol)
	}
	if patch.Confirmations != 0 {
		sets = append(sets, "confirmations=?")
		args = append(args, patch.Confirmations)
	}
	if patch.RpcEndpoint != "" {
		sets = append(sets, "rpc_endpoint=?")
		args = append(args, patch.RpcEndpoint)
	}
	args = append(args, id)
	res, err := s.db.Exec("UPDATE ce_admin_chains SET "+strings.Join(sets, ",")+" WHERE id=?", args...)
	if err != nil {
		return Chain{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Chain{}, ErrCatalogNotFound
	}
	return s.GetChain(id)
}

// ---- 币种 ----

func (s *mysqlCatalogStore) ListCoins() ([]Coin, error) {
	rows, err := s.db.Query(`SELECT id, symbol, name, chain, decimals, withdraw_fee, updated_at
		FROM ce_admin_coins ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Coin{}
	for rows.Next() {
		var c Coin
		if err := rows.Scan(&c.ID, &c.Symbol, &c.Name, &c.Chain, &c.Precision, &c.WithdrawFee, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *mysqlCatalogStore) CreateCoin(coin Coin) (Coin, error) {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_admin_coins (symbol, name, chain, decimals, withdraw_fee, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		coin.Symbol, coin.Name, coin.Chain, coin.Precision, coin.WithdrawFee, now)
	if err != nil {
		return Coin{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Coin{}, err
	}
	coin.ID = id
	coin.UpdatedAt = now
	return coin, nil
}

func (s *mysqlCatalogStore) GetCoin(id int64) (Coin, error) {
	var c Coin
	err := s.db.QueryRow(
		`SELECT id, symbol, name, chain, decimals, withdraw_fee, updated_at
		 FROM ce_admin_coins WHERE id = ?`, id).
		Scan(&c.ID, &c.Symbol, &c.Name, &c.Chain, &c.Precision, &c.WithdrawFee, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return Coin{}, ErrCatalogNotFound
	}
	if err != nil {
		return Coin{}, err
	}
	return c, nil
}

// UpdateCoin 部分更新：字符串仅当非空、Precision/WithdrawFee 仅当非 0 才覆盖。
func (s *mysqlCatalogStore) UpdateCoin(id int64, patch Coin) (Coin, error) {
	sets := []string{"updated_at=NOW(3)"}
	args := []interface{}{}
	if patch.Symbol != "" {
		sets = append(sets, "symbol=?")
		args = append(args, patch.Symbol)
	}
	if patch.Name != "" {
		sets = append(sets, "name=?")
		args = append(args, patch.Name)
	}
	if patch.Chain != "" {
		sets = append(sets, "chain=?")
		args = append(args, patch.Chain)
	}
	if patch.Precision != 0 {
		sets = append(sets, "decimals=?")
		args = append(args, patch.Precision)
	}
	if patch.WithdrawFee != 0 {
		sets = append(sets, "withdraw_fee=?")
		args = append(args, patch.WithdrawFee)
	}
	args = append(args, id)
	res, err := s.db.Exec("UPDATE ce_admin_coins SET "+strings.Join(sets, ",")+" WHERE id=?", args...)
	if err != nil {
		return Coin{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Coin{}, ErrCatalogNotFound
	}
	return s.GetCoin(id)
}

// ---- 本地通知 ----

func (s *mysqlCatalogStore) ListNotifications() ([]Notification, error) {
	rows, err := s.db.Query(`SELECT id, title, body, level, created_at FROM ce_admin_notifications ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.Level, &n.CreatedAt); err != nil {
			return nil, err
		}
		n.Source = "local"
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *mysqlCatalogStore) CreateNotification(n Notification) (Notification, error) {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_admin_notifications (title, body, level, created_at, source) VALUES (?, ?, ?, ?, 'local')`,
		n.Title, n.Body, n.Level, now)
	if err != nil {
		return Notification{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Notification{}, err
	}
	n.ID = id
	n.CreatedAt = now
	n.Source = "local"
	return n, nil
}

func (s *mysqlCatalogStore) DeleteNotification(id int64) error {
	res, err := s.db.Exec(`DELETE FROM ce_admin_notifications WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCatalogNotFound
	}
	return nil
}
