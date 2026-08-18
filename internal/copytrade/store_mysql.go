package copytrade

import (
	"database/sql"
	"strings"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// mysqlStore 是跟单业务的 MySQL 实现。表名 ce_copytrade_leads / ce_copytrade_follows /
// ce_copytrade_copies 遵守 ce_ 前缀约定；ce_copytrade_copies 上的 UNIQUE(event_id, follow_id)
// 提供数据库层 F1 幂等兜底（重复复制直接报重复键，由 service 视为已处理）。
type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 打开 MySQL 并跑迁移（建表），失败返回错误。
func NewMySQLStore(dsn string) (*mysqlStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate.New(db, Migrations).Up(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &mysqlStore{db: db}, nil
}

func (s *mysqlStore) Close() error { return s.db.Close() }

// ---- 带单高手 ----

func (s *mysqlStore) CreateLead(l *LeadTrader) error {
	if l.CreatedAt == 0 {
		l.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO ce_copytrade_leads (id, name, bio, status, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE name=VALUES(name), bio=VALUES(bio), status=VALUES(status)`,
		l.ID, l.Name, l.Bio, string(l.Status), l.CreatedAt)
	return err
}

func (s *mysqlStore) GetLead(id int64) (*LeadTrader, error) {
	row := s.db.QueryRow(`SELECT id, name, bio, status, created_at FROM ce_copytrade_leads WHERE id = ?`, id)
	var l LeadTrader
	var status string
	if err := row.Scan(&l.ID, &l.Name, &l.Bio, &status, &l.CreatedAt); err == sql.ErrNoRows {
		return nil, ErrLeadNotFound
	} else if err != nil {
		return nil, err
	}
	l.Status = LeadStatus(status)
	return &l, nil
}

func (s *mysqlStore) ListActiveLeads() ([]*LeadTrader, error) {
	rows, err := s.db.Query(`SELECT id, name, bio, status, created_at FROM ce_copytrade_leads WHERE status = ? ORDER BY id`, string(LeadActive))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLeads(rows)
}

func (s *mysqlStore) CloseLead(id int64) error {
	_, err := s.db.Exec(`UPDATE ce_copytrade_leads SET status = ? WHERE id = ?`, string(LeadClosed), id)
	return err
}

func (s *mysqlStore) ListAllLeads() ([]*LeadTrader, error) {
	rows, err := s.db.Query(`SELECT id, name, bio, status, created_at FROM ce_copytrade_leads ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLeads(rows)
}

// ---- 跟单关系 ----

func (s *mysqlStore) CreateFollow(f *Follow) error {
	now := time.Now().Unix()
	if f.CreatedAt == 0 {
		f.CreatedAt = now
	}
	// 同一粉丝对同一 lead 仅允许一条 active 关系（应用层去重 + 唯一约束）。
	_, err := s.db.Exec(`INSERT INTO ce_copytrade_follows
		(lead_id, follower_id, copy_ratio, allocated_amount, follower_token, status, created_at, stopped_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)
		ON DUPLICATE KEY UPDATE status='active', copy_ratio=VALUES(copy_ratio), allocated_amount=VALUES(allocated_amount), follower_token=VALUES(follower_token), stopped_at=0`,
		f.LeadID, f.FollowerID, f.CopyRatio, f.AllocatedAmount, f.FollowerToken, string(f.Status), f.CreatedAt)
	if err != nil {
		return err
	}
	// 取回自增 ID（同 key 重插时 ID 不变，简单回查）。
	row := s.db.QueryRow(`SELECT id FROM ce_copytrade_follows WHERE lead_id = ? AND follower_id = ? ORDER BY id DESC LIMIT 1`, f.LeadID, f.FollowerID)
	_ = row.Scan(&f.ID)
	return nil
}

func (s *mysqlStore) GetFollow(id int64) (*Follow, error) {
	row := s.db.QueryRow(`SELECT id, lead_id, follower_id, copy_ratio, allocated_amount, follower_token, status, created_at, stopped_at
		FROM ce_copytrade_follows WHERE id = ?`, id)
	var f Follow
	var status, token string
	if err := row.Scan(&f.ID, &f.LeadID, &f.FollowerID, &f.CopyRatio, &f.AllocatedAmount, &token, &status, &f.CreatedAt, &f.StoppedAt); err == sql.ErrNoRows {
		return nil, ErrFollowNotFound
	} else if err != nil {
		return nil, err
	}
	f.Status = FollowStatus(status)
	f.FollowerToken = token
	return &f, nil
}

func (s *mysqlStore) ListFollowsByLead(leadID int64) ([]*Follow, error) {
	rows, err := s.db.Query(`SELECT id, lead_id, follower_id, copy_ratio, allocated_amount, follower_token, status, created_at, stopped_at
		FROM ce_copytrade_follows WHERE lead_id = ? AND status = ? ORDER BY id`, leadID, string(FollowActive))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFollows(rows)
}

func (s *mysqlStore) ListFollowsByFollower(uid int64) ([]*Follow, error) {
	rows, err := s.db.Query(`SELECT id, lead_id, follower_id, copy_ratio, allocated_amount, follower_token, status, created_at, stopped_at
		FROM ce_copytrade_follows WHERE follower_id = ? ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFollows(rows)
}

func (s *mysqlStore) UpdateFollow(f *Follow) error {
	_, err := s.db.Exec(`UPDATE ce_copytrade_follows SET copy_ratio=?, allocated_amount=?, follower_token=?, status=?, stopped_at=?
		WHERE id = ?`,
		f.CopyRatio, f.AllocatedAmount, f.FollowerToken, string(f.Status), f.StoppedAt, f.ID)
	return err
}

func (s *mysqlStore) ListAllFollows() ([]*Follow, error) {
	rows, err := s.db.Query(`SELECT id, lead_id, follower_id, copy_ratio, allocated_amount, follower_token, status, created_at, stopped_at
		FROM ce_copytrade_follows ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFollows(rows)
}

// ---- 复制成交 ----

func (s *mysqlStore) CreateCopy(c *CopyRecord) error {
	res, err := s.db.Exec(`INSERT INTO ce_copytrade_copies
		(event_id, lead_id, follow_id, follower_id, symbol, side, price, qty, notional, fee_amount, exchange_order_id, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.EventID, c.LeadID, c.FollowID, c.FollowerID, c.Symbol, c.Side, c.Price, c.Qty,
		c.Notional, c.FeeAmount, c.ExchangeOrderID, string(c.Status), c.CreatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		c.ID = id
	}
	return nil
}

func (s *mysqlStore) GetCopy(eventID string, followID int64) (*CopyRecord, error) {
	row := s.db.QueryRow(`SELECT id, event_id, lead_id, follow_id, follower_id, symbol, side, price, qty, notional, fee_amount, exchange_order_id, status, created_at
		FROM ce_copytrade_copies WHERE event_id = ? AND follow_id = ?`, eventID, followID)
	var c CopyRecord
	var status string
	err := row.Scan(&c.ID, &c.EventID, &c.LeadID, &c.FollowID, &c.FollowerID, &c.Symbol, &c.Side,
		&c.Price, &c.Qty, &c.Notional, &c.FeeAmount, &c.ExchangeOrderID, &status, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Status = CopyStatus(status)
	return &c, nil
}

func (s *mysqlStore) ListCopiesByFollower(uid int64) ([]*CopyRecord, error) {
	rows, err := s.db.Query(`SELECT id, event_id, lead_id, follow_id, follower_id, symbol, side, price, qty, notional, fee_amount, exchange_order_id, status, created_at
		FROM ce_copytrade_copies WHERE follower_id = ? ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCopies(rows)
}

func (s *mysqlStore) ListAllCopies() ([]*CopyRecord, error) {
	rows, err := s.db.Query(`SELECT id, event_id, lead_id, follow_id, follower_id, symbol, side, price, qty, notional, fee_amount, exchange_order_id, status, created_at
		FROM ce_copytrade_copies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCopies(rows)
}

// ---- 扫描辅助 ----

func scanLeads(rows *sql.Rows) ([]*LeadTrader, error) {
	out := make([]*LeadTrader, 0)
	for rows.Next() {
		var l LeadTrader
		var status string
		if err := rows.Scan(&l.ID, &l.Name, &l.Bio, &status, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.Status = LeadStatus(status)
		out = append(out, &l)
	}
	return out, rows.Err()
}

func scanFollows(rows *sql.Rows) ([]*Follow, error) {
	out := make([]*Follow, 0)
	for rows.Next() {
		var f Follow
		var status, token string
		if err := rows.Scan(&f.ID, &f.LeadID, &f.FollowerID, &f.CopyRatio, &f.AllocatedAmount, &token, &status, &f.CreatedAt, &f.StoppedAt); err != nil {
			return nil, err
		}
		f.Status = FollowStatus(status)
		f.FollowerToken = token
		out = append(out, &f)
	}
	return out, rows.Err()
}

func scanCopies(rows *sql.Rows) ([]*CopyRecord, error) {
	out := make([]*CopyRecord, 0)
	for rows.Next() {
		var c CopyRecord
		var status string
		if err := rows.Scan(&c.ID, &c.EventID, &c.LeadID, &c.FollowID, &c.FollowerID, &c.Symbol, &c.Side,
			&c.Price, &c.Qty, &c.Notional, &c.FeeAmount, &c.ExchangeOrderID, &status, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Status = CopyStatus(status)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// isDuplicateKey 判断是否为 MySQL 唯一约束冲突（用作 F1 幂等兜底）。
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry")
}
