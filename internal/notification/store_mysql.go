package notification

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// mysqlStore 是通知的 MySQL 实现，表名遵守 ce_ 约定。
type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 创建 MySQL 存储并执行迁移建表（幂等）。
func NewMySQLStore(db *sql.DB) (*mysqlStore, error) {
	if err := migrate.New(db, NotificationMigrations).Up(); err != nil {
		return nil, fmt.Errorf("notification migrate up: %w", err)
	}
	return &mysqlStore{db: db}, nil
}

func (s *mysqlStore) Create(n *Notification) (*Notification, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO ce_notifications (user_id, type, title, body, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		n.UserID, n.Type, n.Title, n.Body, StatusUnread, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	cp := *n
	cp.ID = id
	cp.Status = StatusUnread
	cp.CreatedAt = now
	return &cp, nil
}

func (s *mysqlStore) List(userID int64, onlyUnread bool, limit int) ([]*Notification, error) {
	q := `SELECT id, user_id, type, title, body, status, created_at FROM ce_notifications WHERE user_id = ?`
	if onlyUnread {
		q += ` AND status = 'unread'`
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	return s.query(q, userID)
}

func (s *mysqlStore) ListAll(limit int) ([]*Notification, error) {
	q := `SELECT id, user_id, type, title, body, status, created_at FROM ce_notifications ORDER BY id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	return s.query(q)
}

func (s *mysqlStore) MarkRead(userID, id int64) error {
	res, err := s.db.Exec(
		`UPDATE ce_notifications SET status = 'read' WHERE id = ? AND user_id = ? AND status = 'unread'`,
		id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 可能不存在，也可能已读；查询一次区分。
		var cnt int64
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM ce_notifications WHERE id = ? AND user_id = ?`, id, userID).Scan(&cnt)
		if cnt == 0 {
			return ErrNotFound
		}
	}
	return nil
}

func (s *mysqlStore) MarkAllRead(userID int64) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE ce_notifications SET status = 'read' WHERE user_id = ? AND status = 'unread'`, userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *mysqlStore) CountUnread(userID int64) (int64, error) {
	var cnt int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM ce_notifications WHERE user_id = ? AND status = 'unread'`, userID).Scan(&cnt); err != nil {
		return 0, err
	}
	return cnt, nil
}

func (s *mysqlStore) Delete(userID, id int64) error {
	res, err := s.db.Exec(
		`DELETE FROM ce_notifications WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mysqlStore) query(q string, args ...interface{}) ([]*Notification, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Notification, 0)
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.Type, &n.Title, &n.Body, &n.Status, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}
