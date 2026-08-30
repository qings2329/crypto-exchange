package adminapi

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// 审计日志迁移版本（错开账户 92xx、公告 94xx、理财 97xx 等）。
const adminMigVerAudit = 9812

// AuditMigrations 是审计日志表的建表迁移，由 NewMySQLAuditStore 应用。
var AuditMigrations = []migrate.Migration{
	{
		Version: adminMigVerAudit,
		Name:    "create_ce_admin_audit_logs",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_audit_logs (
    id       BIGINT      NOT NULL AUTO_INCREMENT,
    admin_id BIGINT      NOT NULL,
    method   VARCHAR(8)  NOT NULL,
    path     VARCHAR(255) NOT NULL,
    action   VARCHAR(16) NOT NULL DEFAULT '',
    target   VARCHAR(255) NOT NULL DEFAULT '',
    status   INT         NOT NULL DEFAULT 0,
    detail   VARCHAR(255) NOT NULL DEFAULT '',
    ip       VARCHAR(64) NOT NULL DEFAULT '',
    time     BIGINT      NOT NULL,
    PRIMARY KEY (id),
    KEY idx_time (time),
    KEY idx_admin (admin_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_audit_logs;",
	},
}

type mysqlAuditStore struct {
	db *sql.DB
}

// NewMySQLAuditStore 以 DSN 打开连接并应用迁移，返回 MySQL 实现。
func NewMySQLAuditStore(dsn string) (AuditStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := migrate.New(db, AuditMigrations).Up(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("audit migrations: %w", err)
	}
	if _, err := db.Exec("SELECT 1 FROM ce_admin_audit_logs LIMIT 1"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("audit table not ready: %w", err)
	}
	return &mysqlAuditStore{db: db}, nil
}

func (s *mysqlAuditStore) Append(e AuditEntry) error {
	if e.Time == 0 {
		e.Time = time.Now().UnixNano()
	}
	res, err := s.db.Exec(
		`INSERT INTO ce_admin_audit_logs (admin_id, method, path, action, target, status, detail, ip, time)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.AdminID, e.Method, e.Path, e.Action, e.Target, e.Status, e.Detail, e.IP, e.Time)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	e.ID = id
	return nil
}

func (s *mysqlAuditStore) List(limit, offset int, f AuditFilter) ([]AuditEntry, int64, error) {
	where, args := auditWhere(f)
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ce_admin_audit_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `SELECT id, admin_id, method, path, action, target, status, detail, ip, time
		FROM ce_admin_audit_logs` + where + ` ORDER BY time DESC, id DESC`
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
		if offset > 0 {
			query += " OFFSET ?"
			args = append(args, offset)
		}
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.AdminID, &e.Method, &e.Path, &e.Action, &e.Target, &e.Status, &e.Detail, &e.IP, &e.Time); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// auditWhere 根据过滤条件构造 WHERE 子句与参数（无过滤时返回空字符串）。
func auditWhere(f AuditFilter) (string, []interface{}) {
	conds := []string{}
	args := []interface{}{}
	if f.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, f.Action)
	}
	if f.Method != "" {
		conds = append(conds, "method = ?")
		args = append(args, f.Method)
	}
	if f.AdminID > 0 {
		conds = append(conds, "admin_id = ?")
		args = append(args, f.AdminID)
	}
	if f.Keyword != "" {
		conds = append(conds, "(path LIKE ? OR target LIKE ? OR method LIKE ?)")
		kw := "%" + f.Keyword + "%"
		args = append(args, kw, kw, kw)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + joinConds(conds), args
}

// joinConds 以 AND 连接过滤条件。
func joinConds(conds []string) string {
	out := conds[0]
	for _, c := range conds[1:] {
		out += " AND " + c
	}
	return out
}
