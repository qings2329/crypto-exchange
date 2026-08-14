package risk

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// mysqlStore 是风控的 MySQL 实现，表名遵守 ce_ 约定。
type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 创建 MySQL 存储并执行迁移建表（幂等）。
func NewMySQLStore(db *sql.DB) (*mysqlStore, error) {
	if err := migrate.New(db, RiskMigrations).Up(); err != nil {
		return nil, fmt.Errorf("risk migrate up: %w", err)
	}
	return &mysqlStore{db: db}, nil
}

func (s *mysqlStore) UpsertRule(r *RiskRule) (*RiskRule, error) {
	now := time.Now().UTC()
	if r.ID > 0 {
		res, err := s.db.Exec(
			`UPDATE ce_risk_rules SET name=?, kind=?, scope=?, user_id=?, asset=?,
				max_amount_per_day=?, max_count_per_day=?, min_kyc_level=?, enabled=?
			 WHERE id=?`,
			r.Name, r.Kind, r.Scope, r.UserID, r.Asset, r.MaxAmountPerDay, r.MaxCountPerDay,
			r.MinKYCLevel, boolToInt(r.Enabled), r.ID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrNotFound
		}
		cp := *r
		return &cp, nil
	}
	res, err := s.db.Exec(
		`INSERT INTO ce_risk_rules
		 (name, kind, scope, user_id, asset, max_amount_per_day, max_count_per_day, min_kyc_level, enabled, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.Name, r.Kind, r.Scope, r.UserID, r.Asset, r.MaxAmountPerDay, r.MaxCountPerDay,
		r.MinKYCLevel, boolToInt(r.Enabled), now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	cp := *r
	cp.ID = id
	cp.CreatedAt = now
	return &cp, nil
}

func (s *mysqlStore) GetRule(id int64) (*RiskRule, error) {
	return s.scanRule(`SELECT id,name,kind,scope,user_id,asset,max_amount_per_day,max_count_per_day,min_kyc_level,enabled,created_at
		FROM ce_risk_rules WHERE id=?`, id)
}

func (s *mysqlStore) ListRules(kind string) ([]*RiskRule, error) {
	q := `SELECT id,name,kind,scope,user_id,asset,max_amount_per_day,max_count_per_day,min_kyc_level,enabled,created_at FROM ce_risk_rules`
	var args []interface{}
	if kind != "" {
		q += ` WHERE kind=?`
		args = append(args, kind)
	}
	return s.queryRules(q, args...)
}

func (s *mysqlStore) AddBlacklist(b *BlacklistEntry) (*BlacklistEntry, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO ce_risk_blacklist (target, kind, reason, created_at) VALUES (?,?,?,?)
		 ON DUPLICATE KEY UPDATE reason=VALUES(reason), created_at=VALUES(created_at)`,
		b.Target, b.Kind, b.Reason, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	cp := *b
	cp.ID = id
	cp.CreatedAt = now
	return &cp, nil
}

func (s *mysqlStore) RemoveBlacklist(target string) error {
	res, err := s.db.Exec(`DELETE FROM ce_risk_blacklist WHERE target=?`, target)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mysqlStore) IsBlacklisted(target string) (bool, error) {
	var cnt int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ce_risk_blacklist WHERE target=?`, target).Scan(&cnt); err != nil {
		return false, err
	}
	return cnt > 0, nil
}

func (s *mysqlStore) ListBlacklist(kind string) ([]*BlacklistEntry, error) {
	q := `SELECT id,target,kind,reason,created_at FROM ce_risk_blacklist`
	var args []interface{}
	if kind != "" {
		q += ` WHERE kind=?`
		args = append(args, kind)
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*BlacklistEntry, 0)
	for rows.Next() {
		var b BlacklistEntry
		if err := rows.Scan(&b.ID, &b.Target, &b.Kind, &b.Reason, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

func (s *mysqlStore) RecordEvent(e *RiskEvent) (*RiskEvent, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO ce_risk_events (user_id, kind, detail, created_at) VALUES (?,?,?,?)`,
		e.UserID, e.Kind, e.Detail, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	cp := *e
	cp.ID = id
	cp.CreatedAt = now
	return &cp, nil
}

func (s *mysqlStore) ListEvents(userID int64, limit int) ([]*RiskEvent, error) {
	q := `SELECT id,user_id,kind,detail,created_at FROM ce_risk_events`
	var args []interface{}
	if userID != 0 {
		q += ` WHERE user_id=?`
		args = append(args, userID)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*RiskEvent, 0)
	for rows.Next() {
		var e RiskEvent
		if err := rows.Scan(&e.ID, &e.UserID, &e.Kind, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *mysqlStore) scanRule(q string, args ...interface{}) (*RiskRule, error) {
	var r RiskRule
	var enabled int
	if err := s.db.QueryRow(q, args...).Scan(
		&r.ID, &r.Name, &r.Kind, &r.Scope, &r.UserID, &r.Asset,
		&r.MaxAmountPerDay, &r.MaxCountPerDay, &r.MinKYCLevel, &enabled, &r.CreatedAt); err == sql.ErrNoRows {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	r.Enabled = enabled == 1
	return &r, nil
}

func (s *mysqlStore) queryRules(q string, args ...interface{}) ([]*RiskRule, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*RiskRule, 0)
	for rows.Next() {
		var r RiskRule
		var enabled int
		if err := rows.Scan(
			&r.ID, &r.Name, &r.Kind, &r.Scope, &r.UserID, &r.Asset,
			&r.MaxAmountPerDay, &r.MaxCountPerDay, &r.MinKYCLevel, &enabled, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
