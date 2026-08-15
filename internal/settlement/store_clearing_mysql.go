package settlement

import (
	"database/sql"
	"fmt"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// mysqlClearingStore 以 MySQL 为后端持久化清算流水（ce_settlement_trades）。
// 幂等由主键 id + INSERT IGNORE 保证：重复成交（Kafka at-least-once）写入 0 行，
// Record 返回 inserted=false。
type mysqlClearingStore struct {
	db *sql.DB
}

// NewMySQLClearingStore 以 DSN 打开连接并应用迁移，返回 MySQL 实现。
func NewMySQLClearingStore(dsn string) (ClearingStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := migrate.New(db, ClearingMigrations).Up(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("clearing migrations: %w", err)
	}
	if _, err := db.Exec("SELECT 1 FROM ce_settlement_trades LIMIT 1"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("clearing table not ready: %w", err)
	}
	return &mysqlClearingStore{db: db}, nil
}

func (s *mysqlClearingStore) Record(t ClearedTrade) (bool, error) {
	res, err := s.db.Exec(
		`INSERT IGNORE INTO ce_settlement_trades
		 (id, symbol, price, qty, taker_id, maker_id, taker_side, fee, ts, cleared_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Symbol, t.Price, t.Qty, t.TakerID, t.MakerID, t.TakerSide, t.Fee, t.Ts, t.ClearedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *mysqlClearingStore) Recent(limit int) ([]ClearedTrade, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, symbol, price, qty, taker_id, maker_id, taker_side, fee, ts, cleared_at
		 FROM ce_settlement_trades ORDER BY cleared_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ClearedTrade, 0, limit)
	for rows.Next() {
		var t ClearedTrade
		if err := rows.Scan(&t.ID, &t.Symbol, &t.Price, &t.Qty, &t.TakerID, &t.MakerID,
			&t.TakerSide, &t.Fee, &t.Ts, &t.ClearedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *mysqlClearingStore) Close() error {
	return s.db.Close()
}

// NewClearingStore 优先返回 MySQL 实现；DSN 为空或连接/迁移失败则回退内存实现。
func NewClearingStore(dsn string) (store ClearingStore, isMem bool, err error) {
	if dsn == "" {
		return NewMemClearingStore(0), true, nil
	}
	ms, e := NewMySQLClearingStore(dsn)
	if e != nil {
		return NewMemClearingStore(0), true, e
	}
	return ms, false, nil
}
