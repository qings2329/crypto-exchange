package spot

import (
	"database/sql"
	"fmt"
	"math/big"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// OrderRecord 是 spot 一笔未完结订单的持久化快照（预冻结记录）。用于在进程重启后重建
// clientOIDMap（下单幂等检查），使相同 client_oid 的重试在重启后仍能被识别、不再重复预冻结
// 与重复提交（修复重启+重试双冻）。
//
// 注意：完整重启安全还需 spot 账本本身的持久化（当前 spot 账本为进程内内存，重启即清零，
// 见 cmd/spot）。因此本存储当前仅用于重建「幂等映射」；openOrders（冻结释放）的重载依赖账本
// 持久化这一前置特性，待 spot 账本持久化落地后再启用，届时 RestoreOrders 一并重建 openOrders。
type OrderRecord struct {
	OrderID     int64
	User        int64
	Side        int // matching.Side
	Symbol      string
	Base        string
	Quote       string
	FrozenQuote settlement.AssetAmount
	FrozenBase  settlement.AssetAmount
	ClientOID   string
}

// Store 持久化 spot 订单（用于重启后恢复幂等映射）。nil 表示不持久化（纯内存演示，重启间隙缺口仍在）。
type Store interface {
	UpsertOrder(r OrderRecord) error
	DeleteOrder(orderID int64) error
	LoadOrders() ([]OrderRecord, error)
}

// SpotMigrations 建表迁移。版本号使用 98xx 段（全局唯一，避免与其他模块冲突，见 migrate 包约定）。
var SpotMigrations = []migrate.Migration{
	{
		Version: 9801,
		Name:    "create_ce_spot_orders",
		Up: `CREATE TABLE IF NOT EXISTS ce_spot_orders (
				order_id         BIGINT       NOT NULL,
				user_id          BIGINT       NOT NULL,
				side             INT         NOT NULL,
				symbol           VARCHAR(32) NOT NULL,
				base             VARCHAR(32) NOT NULL,
				quote            VARCHAR(32) NOT NULL,
				frozen_quote_val VARCHAR(64) NOT NULL DEFAULT '0',
				frozen_quote_dec INT         NOT NULL DEFAULT 0,
				frozen_base_val  VARCHAR(64) NOT NULL DEFAULT '0',
				frozen_base_dec  INT         NOT NULL DEFAULT 0,
				client_oid       VARCHAR(128) NOT NULL DEFAULT '',
				created_at       DATETIME(3) NOT NULL,
				PRIMARY KEY (order_id),
				INDEX idx_user (user_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_spot_orders`,
	},
}

type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 打开 MySQL、跑迁移，返回 MySQL 版 Store。
func NewMySQLStore(dsn string) (*mysqlStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate.New(db, SpotMigrations).Up(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &mysqlStore{db: db}, nil
}

func (s *mysqlStore) UpsertOrder(r OrderRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO ce_spot_orders
			(order_id, user_id, side, symbol, base, quote,
			 frozen_quote_val, frozen_quote_dec, frozen_base_val, frozen_base_dec, client_oid, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(3))
		ON DUPLICATE KEY UPDATE
			user_id=VALUES(user_id), side=VALUES(side), symbol=VALUES(symbol),
			base=VALUES(base), quote=VALUES(quote),
			frozen_quote_val=VALUES(frozen_quote_val), frozen_quote_dec=VALUES(frozen_quote_dec),
			frozen_base_val=VALUES(frozen_base_val), frozen_base_dec=VALUES(frozen_base_dec),
			client_oid=VALUES(client_oid)`,
		r.OrderID, r.User, r.Side, r.Symbol, r.Base, r.Quote,
		r.FrozenQuote.Value.String(), r.FrozenQuote.Decimals,
		r.FrozenBase.Value.String(), r.FrozenBase.Decimals,
		r.ClientOID)
	return err
}

func (s *mysqlStore) DeleteOrder(orderID int64) error {
	_, err := s.db.Exec("DELETE FROM ce_spot_orders WHERE order_id = ?", orderID)
	return err
}

func (s *mysqlStore) LoadOrders() ([]OrderRecord, error) {
	rows, err := s.db.Query(`SELECT order_id, user_id, side, symbol, base, quote,
		frozen_quote_val, frozen_quote_dec, frozen_base_val, frozen_base_dec, client_oid
		FROM ce_spot_orders`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OrderRecord, 0)
	for rows.Next() {
		var r OrderRecord
		var fqV, fbV string
		var fqD, fbD int
		if err := rows.Scan(&r.OrderID, &r.User, &r.Side, &r.Symbol, &r.Base, &r.Quote,
			&fqV, &fqD, &fbV, &fbD, &r.ClientOID); err != nil {
			return nil, err
		}
		r.FrozenQuote = settlement.AssetAmount{Value: new(big.Int), Decimals: fqD}
		r.FrozenQuote.Value.SetString(fqV, 10)
		r.FrozenBase = settlement.AssetAmount{Value: new(big.Int), Decimals: fbD}
		r.FrozenBase.Value.SetString(fbV, 10)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RestoreOrders 由持久化记录重建 clientOIDMap（幂等映射），使重启后同 client_oid 重试仍被识别。
// 当前仅重建 clientOIDMap；openOrders 重载依赖 spot 账本持久化（前置特性），暂不重建。
func (s *Server) RestoreOrders(records []OrderRecord) {
	s.freezeMu.Lock()
	defer s.freezeMu.Unlock()
	for _, r := range records {
		if r.ClientOID != "" {
			s.clientOIDMap[fmt.Sprintf("%d:%s", r.User, r.ClientOID)] = r.OrderID
		}
	}
}

// freezeRecToRecord 由预冻结记录构造待持久化的 OrderRecord。freezeRec 与 OrderRecord 同属 spot
// 包，故可直接访问未导出字段；clientOID 由调用方透传（下单时登记、结算递减重 Upsert 时保留）。
func freezeRecToRecord(orderID int64, rec *freezeRec, clientOID string) OrderRecord {
	return OrderRecord{
		OrderID:     orderID,
		User:        rec.user,
		Side:        int(rec.side),
		Symbol:      rec.symbol,
		Base:        rec.base,
		Quote:       rec.quote,
		FrozenQuote: rec.frozenQuote,
		FrozenBase:  rec.frozenBase,
		ClientOID:   clientOID,
	}
}
