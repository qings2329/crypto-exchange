package apikeys

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// API Key 迁移版本（错开账户 92xx、公告 94xx、审计 9801、理财 97xx 等）。
const apiKeyMigVer = 9802

// Migrations 是 ce_admin_api_keys 表的建表迁移，由 NewMySQLStore 应用。
var Migrations = []migrate.Migration{
	{
		Version: apiKeyMigVer,
		Name:    "create_ce_admin_api_keys",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_api_keys (
    id          BIGINT       NOT NULL AUTO_INCREMENT,
    user_id     BIGINT       NOT NULL,
    label       VARCHAR(64)  NOT NULL,
    prefix      VARCHAR(16)  NOT NULL,
    key_hash    VARCHAR(64)  NOT NULL,
    permissions TEXT         NOT NULL,
    status      VARCHAR(16)  NOT NULL DEFAULT 'active',
    created_by  BIGINT       NOT NULL DEFAULT 0,
    created_at  BIGINT       NOT NULL,
    last_used_at BIGINT      NULL,
    revoked_at  BIGINT       NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_key_hash (key_hash),
    KEY idx_user (user_id),
    KEY idx_prefix (prefix)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_api_keys;",
	},
}

type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 以 DSN 打开连接并应用迁移，返回 MySQL 实现。
func NewMySQLStore(dsn string) (Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := migrate.New(db, Migrations).Up(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apikeys migrations: %w", err)
	}
	if _, err := db.Exec("SELECT 1 FROM ce_admin_api_keys LIMIT 1"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apikeys table not ready: %w", err)
	}
	return &mysqlStore{db: db}, nil
}

func permsToJSON(p []string) string {
	if p == nil {
		p = []string{}
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func jsonToPerms(s string) []string {
	var out []string
	if s == "" {
		return []string{}
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func (s *mysqlStore) Create(k *APIKey) error {
	if k.UserID == 0 || k.Label == "" || k.Prefix == "" || k.KeyHash == "" {
		return ErrInvalidInput
	}
	res, err := s.db.Exec(
		`INSERT INTO ce_admin_api_keys (user_id, label, prefix, key_hash, permissions, status, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.UserID, k.Label, k.Prefix, k.KeyHash, permsToJSON(k.Permissions), StatusActive, k.CreatedBy, k.CreatedAt.Unix(),
	)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	k.ID = id
	k.Status = StatusActive
	return nil
}

func scanKey(row interface {
	Scan(dest ...interface{}) error
}) (*APIKey, error) {
	var (
		k           APIKey
		perms       string
		createdAt   int64
		lastUsedAt  sql.NullInt64
		revokedAt   sql.NullInt64
	)
	if err := row.Scan(&k.ID, &k.UserID, &k.Label, &k.Prefix, &k.KeyHash, &perms, &k.Status, &k.CreatedBy, &createdAt, &lastUsedAt, &revokedAt); err != nil {
		return nil, err
	}
	k.Permissions = jsonToPerms(perms)
	k.CreatedAt = time.Unix(createdAt, 0).UTC()
	if lastUsedAt.Valid {
		t := time.Unix(lastUsedAt.Int64, 0).UTC()
		k.LastUsedAt = &t
	}
	if revokedAt.Valid {
		t := time.Unix(revokedAt.Int64, 0).UTC()
		k.RevokedAt = &t
	}
	return &k, nil
}

func (s *mysqlStore) GetByID(id int64) (*APIKey, error) {
	row := s.db.QueryRow(
		`SELECT id, user_id, label, prefix, key_hash, permissions, status, created_by, created_at, last_used_at, revoked_at
		 FROM ce_admin_api_keys WHERE id = ?`, id)
	k, err := scanKey(row)
	if err == sql.ErrNoRows {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}

func (s *mysqlStore) List(f ListFilter) ([]*APIKey, error) {
	query := `SELECT id, user_id, label, prefix, key_hash, permissions, status, created_by, created_at, last_used_at, revoked_at
		FROM ce_admin_api_keys`
	args := []interface{}{}
	if f.UserID != 0 {
		query += " WHERE user_id = ?"
		args = append(args, f.UserID)
	}
	query += " ORDER BY created_at DESC, id DESC"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *mysqlStore) ListByUser(userID int64) ([]*APIKey, error) {
	return s.List(ListFilter{UserID: userID})
}

func (s *mysqlStore) GetByKeyHash(keyHash string) (*APIKey, error) {
	row := s.db.QueryRow(
		`SELECT id, user_id, label, prefix, key_hash, permissions, status, created_by, created_at, last_used_at, revoked_at
		 FROM ce_admin_api_keys WHERE key_hash = ?`, keyHash)
	k, err := scanKey(row)
	if err == sql.ErrNoRows {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}

func (s *mysqlStore) Revoke(id int64) error {
	res, err := s.db.Exec(
		`UPDATE ce_admin_api_keys SET status = ?, revoked_at = ? WHERE id = ? AND status = ?`,
		StatusRevoked, time.Now().Unix(), id, StatusActive)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// 区分「不存在」与「已吊销」
		if _, gerr := s.GetByID(id); gerr != nil {
			return ErrKeyNotFound
		}
		return ErrKeyRevoked
	}
	return nil
}

// NewStore 优先返回 MySQL 实现；若 DSN 为空或连接/迁移失败，则回退内存实现。
func NewStore(dsn string) Store {
	if dsn == "" {
		return NewMemStore()
	}
	ms, err := NewMySQLStore(dsn)
	if err != nil {
		return NewMemStore()
	}
	return ms
}
