package c2c

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 构造 MySQL 存储；连接或迁移失败时返回错误（由调用方决定降级到内存）。
func NewMySQLStore(dsn string) (Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	s := &mysqlStore{db: db}
	if err := s.runMigrations(); err != nil {
		return nil, fmt.Errorf("c2c migrations: %w", err)
	}
	return s, nil
}

func (s *mysqlStore) runMigrations() error {
	return migrate.New(s.db, []migrate.Migration{
		{
			Version: 9607,
			Name:    "create_ce_c2c_orders",
			Up: `CREATE TABLE IF NOT EXISTS ce_c2c_orders (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    side VARCHAR(8) NOT NULL COMMENT 'buy/sell',
    coin VARCHAR(32) NOT NULL,
    amount DECIMAL(30,8) NOT NULL DEFAULT 0,
    price DECIMAL(30,8) NOT NULL DEFAULT 0,
    total DECIMAL(30,8) NOT NULL DEFAULT 0,
    user_id BIGINT NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL DEFAULT 'open',
    note VARCHAR(255) NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    KEY idx_user_id (user_id),
    KEY idx_status (status),
    KEY idx_coin (coin)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
			Down: "DROP TABLE IF EXISTS ce_c2c_orders;",
		},
	}).Up()
}

func (s *mysqlStore) Create(o *Order) error {
	res, err := s.db.Exec(
		`INSERT INTO ce_c2c_orders (side, coin, amount, price, total, user_id, status, note)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(o.Side), o.Coin, o.Amount, o.Price, o.Total, o.UserID, string(o.Status), o.Note)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	o.ID = id
	return nil
}

// orderColumn 是 ORDER BY 排序顺序的可信白名单（防止 SQL 注入）。
// 当前仅支持按创建时间倒序/正序。
func filterWhere(f OrderFilter) (string, []interface{}) {
	var conds []string
	var args []interface{}
	if f.UserID != 0 {
		conds = append(conds, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.Side != "" {
		conds = append(conds, "side = ?")
		args = append(args, string(f.Side))
	}
	if f.Coin != "" {
		conds = append(conds, "coin = ?")
		args = append(args, f.Coin)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, string(f.Status))
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (s *mysqlStore) List(filter OrderFilter, limit, offset int) ([]*Order, int, error) {
	where, args := filterWhere(filter)

	var total int
	countSQL := `SELECT COUNT(*) FROM ce_c2c_orders` + where
	if err := s.db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("c2c count: %w", err)
	}

	query := `SELECT id, side, coin, amount, price, total, user_id, status, note, created_at, updated_at
		FROM ce_c2c_orders` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*Order
	for rows.Next() {
		o := &Order{}
		var side, coin, status, note string
		if err := rows.Scan(&o.ID, &side, &coin, &o.Amount, &o.Price, &o.Total, &o.UserID, &status, &note, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, 0, err
		}
		o.Side = Side(side)
		o.Coin = coin
		o.Status = OrderStatus(status)
		o.Note = note
		out = append(out, o)
	}
	return out, total, rows.Err()
}

func (s *mysqlStore) Get(id int64) (*Order, error) {
	o := &Order{}
	var side, coin, status, note string
	err := s.db.QueryRow(
		`SELECT id, side, coin, amount, price, total, user_id, status, note, created_at, updated_at
		 FROM ce_c2c_orders WHERE id = ?`, id).
		Scan(&o.ID, &side, &coin, &o.Amount, &o.Price, &o.Total, &o.UserID, &status, &note, &o.CreatedAt, &o.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.Side = Side(side)
	o.Coin = coin
	o.Status = OrderStatus(status)
	o.Note = note
	return o, nil
}

func (s *mysqlStore) UpdateStatus(id int64, from, to OrderStatus) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE ce_c2c_orders SET status = ? WHERE id = ? AND status = ?`,
		string(to), id, string(from))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// 可能是目标订单不存在，也可能是状态已被并发修改。
		if _, err := s.Get(id); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func (s *mysqlStore) Update(o *Order) error {
	res, err := s.db.Exec(
		`UPDATE ce_c2c_orders SET side=?, coin=?, amount=?, price=?, total=?, note=? WHERE id=?`,
		string(o.Side), o.Coin, o.Amount, o.Price, o.Total, o.Note, o.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if _, err := s.Get(o.ID); err != nil {
			return err
		}
		return nil
	}
	return nil
}
