package referral

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

type mysqlStore struct {
	db *sql.DB
}

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
		return nil, fmt.Errorf("referral migrations: %w", err)
	}
	return s, nil
}
func (s *mysqlStore) runMigrations() error {
	return migrate.New(s.db, []migrate.Migration{
		{
			Version: 9301,
			Name:    "create_ce_referral_commissions",
			Up: `CREATE TABLE IF NOT EXISTS ce_referral_commissions (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    referrer_id BIGINT NOT NULL,
    taker_id BIGINT NOT NULL,
    asset VARCHAR(32) NOT NULL,
    amount BIGINT NOT NULL DEFAULT 0,
    rate DOUBLE NOT NULL DEFAULT 0,
    status TINYINT NOT NULL DEFAULT 0 COMMENT '0=pending,1=confirmed',
    biz_ref VARCHAR(128) NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_biz_ref (biz_ref),
    KEY idx_referrer_id (referrer_id),
    KEY idx_taker_id (taker_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
			Down: "DROP TABLE IF EXISTS ce_referral_commissions;",
		},
	}).Up()
}

func (s *mysqlStore) RecordCommission(c *ReferralCommission) error {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT IGNORE INTO ce_referral_commissions (referrer_id, taker_id, asset, amount, rate, status, biz_ref, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ReferrerID, c.TakerID, c.Asset, c.Amount, c.Rate, int(c.Status), c.BizRef, now, now)
	if err != nil {
		if isDuplicate(err) {
			return ErrCommissionExists
		}
		return err
	}
	id, _ := res.LastInsertId()
	c.ID = id
	c.CreatedAt = now
	c.UpdatedAt = now
	return nil
}

func (s *mysqlStore) GetCommissionByRef(bizRef string) (*ReferralCommission, error) {
	c := &ReferralCommission{}
	err := s.db.QueryRow(
		`SELECT id, referrer_id, taker_id, asset, amount, rate, status, biz_ref, created_at, updated_at
		 FROM ce_referral_commissions WHERE biz_ref = ?`, bizRef).Scan(
		&c.ID, &c.ReferrerID, &c.TakerID, &c.Asset, &c.Amount, &c.Rate, &c.Status, &c.BizRef, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("commission not found")
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *mysqlStore) ListCommissionsByReferrer(referrerID int64, limit, offset int) ([]*ReferralCommission, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ce_referral_commissions WHERE referrer_id = ?`, referrerID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count commissions by referrer: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT id, referrer_id, taker_id, asset, amount, rate, status, biz_ref, created_at, updated_at
		 FROM ce_referral_commissions WHERE referrer_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		referrerID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*ReferralCommission
	for rows.Next() {
		c := &ReferralCommission{}
		if err := rows.Scan(&c.ID, &c.ReferrerID, &c.TakerID, &c.Asset, &c.Amount, &c.Rate, &c.Status, &c.BizRef, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

func (s *mysqlStore) ListAll(limit, offset int) ([]*ReferralCommission, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ce_referral_commissions`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count all commissions: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT id, referrer_id, taker_id, asset, amount, rate, status, biz_ref, created_at, updated_at
		 FROM ce_referral_commissions ORDER BY id DESC LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*ReferralCommission
	for rows.Next() {
		c := &ReferralCommission{}
		if err := rows.Scan(&c.ID, &c.ReferrerID, &c.TakerID, &c.Asset, &c.Amount, &c.Rate, &c.Status, &c.BizRef, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

func (s *mysqlStore) TotalByReferrer(referrerID int64) (map[string]int64, error) {
	rows, err := s.db.Query(
		`SELECT asset, SUM(amount) as total FROM ce_referral_commissions WHERE referrer_id = ? AND status = 1 GROUP BY asset`,
		referrerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var asset string
		var total int64
		if err := rows.Scan(&asset, &total); err != nil {
			return nil, err
		}
		out[asset] = total
	}
	return out, rows.Err()
}

func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	me, ok := err.(*mysql.MySQLError)
	return ok && me.Number == 1062
}
