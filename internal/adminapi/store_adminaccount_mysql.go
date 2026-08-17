package adminapi

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// 管理员模块迁移版本号（错开 user 的 91xx、ledger 的 90xx）。
const (
	adminMigVerAccounts = 9201
	adminMigVerRoles    = 9202
	adminMigVerRolePerms = 9203
	adminMigVerLoginLock = 9204
)

// AdminMigrations 是管理员模块的建表迁移，运行时由 NewMySQLAdminStore 应用。
var AdminMigrations = []migrate.Migration{
	{
		Version: adminMigVerAccounts,
		Name:    "create_ce_admin_accounts",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_accounts (
    id            BIGINT       NOT NULL AUTO_INCREMENT,
    username      VARCHAR(64)  NOT NULL,
    pass_hash     VARCHAR(255) NOT NULL,
    status        VARCHAR(16)  NOT NULL DEFAULT 'pending',
    role_id       BIGINT       NOT NULL DEFAULT 0,
    totp_secret   VARCHAR(255) NULL,
    totp_enabled  TINYINT      NOT NULL DEFAULT 0,
    created_at    DATETIME(3)  NOT NULL,
    updated_at    DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_accounts;",
	},
	{
		Version: adminMigVerRoles,
		Name:    "create_ce_admin_roles",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_roles (
    id          BIGINT       NOT NULL AUTO_INCREMENT,
    name        VARCHAR(64)  NOT NULL,
    description VARCHAR(255) NOT NULL DEFAULT '',
    created_at  DATETIME(3)  NOT NULL,
    updated_at  DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_role_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_roles;",
	},
	{
		Version: adminMigVerRolePerms,
		Name:    "create_ce_admin_role_perms",
		Up: `CREATE TABLE IF NOT EXISTS ce_admin_role_perms (
    role_id   BIGINT      NOT NULL,
    perm_key  VARCHAR(64) NOT NULL,
    PRIMARY KEY (role_id, perm_key),
    KEY idx_perm_key (perm_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_admin_role_perms;",
	},
	{
		// 登录暴力防护：在已存在的账户表上追加失败计数与锁定到期列（幂等，仅首次应用）。
		Version: adminMigVerLoginLock,
		Name:    "alter_ce_admin_accounts_login_lock",
		Up: `ALTER TABLE ce_admin_accounts
    ADD COLUMN failed_attempts INT    NOT NULL DEFAULT 0,
    ADD COLUMN locked_until   BIGINT NOT NULL DEFAULT 0;`,
		Down: `ALTER TABLE ce_admin_accounts
    DROP COLUMN failed_attempts,
    DROP COLUMN locked_until;`,
	},
}

type mysqlAdminStore struct {
	db *sql.DB
}

// NewMySQLAdminStore 以 DSN 打开连接并应用迁移，返回 MySQL 实现。
func NewMySQLAdminStore(dsn string) (AdminStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := migrate.New(db, AdminMigrations).Up(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("admin migrations: %w", err)
	}
	// 探测关键表是否真实存在：防御 migrate 版本记录与物理表不一致的脏状态
	// （版本已记录但建表失败），避免返回"成功"却无法读写。表缺失则报错，
	// 上层 NewAdminStore 会回退内存实现（本地无干净 MySQL 时可运行）。
	if _, err := db.Exec("SELECT 1 FROM ce_admin_accounts LIMIT 1"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("admin tables not ready: %w", err)
	}
	return &mysqlAdminStore{db: db}, nil
}

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1062
	}
	return false
}

func (s *mysqlAdminStore) CountAccounts() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ce_admin_accounts`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *mysqlAdminStore) CreateAccount(a *AdminAccount) error {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_admin_accounts (username, pass_hash, status, role_id, totp_secret, totp_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)`,
		a.Username, a.PasswordHash, a.Status, a.RoleID, a.TOTPSecret, boolToInt(a.TOTPEnabled), now, now)
	if err != nil {
		if isDuplicate(err) {
			return ErrAdminExists
		}
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	a.ID = id
	a.CreatedAt = now
	a.UpdatedAt = now
	return nil
}

func (s *mysqlAdminStore) GetAccountByUsername(username string) (*AdminAccount, error) {
	return s.scanAccount(
		`SELECT id, username, pass_hash, status, role_id, COALESCE(totp_secret,''), totp_enabled,
		        failed_attempts, locked_until, created_at, updated_at
		 FROM ce_admin_accounts WHERE username = ?`, username)
}

func (s *mysqlAdminStore) GetAccountByID(id int64) (*AdminAccount, error) {
	return s.scanAccount(
		`SELECT id, username, pass_hash, status, role_id, COALESCE(totp_secret,''), totp_enabled,
		        failed_attempts, locked_until, created_at, updated_at
		 FROM ce_admin_accounts WHERE id = ?`, id)
}

func (s *mysqlAdminStore) scanAccount(query string, args ...interface{}) (*AdminAccount, error) {
	var a AdminAccount
	var totpSecret string
	var totpEnabled int
	err := s.db.QueryRow(query, args...).Scan(
		&a.ID, &a.Username, &a.PasswordHash, &a.Status, &a.RoleID, &totpSecret, &totpEnabled,
		&a.FailedAttempts, &a.LockedUntil, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrAdminNotFound
	}
	if err != nil {
		return nil, err
	}
	a.TOTPSecret = totpSecret
	a.TOTPEnabled = totpEnabled == 1
	return &a, nil
}

func (s *mysqlAdminStore) ListAccounts() ([]*AdminAccount, error) {
	rows, err := s.db.Query(
		`SELECT id, username, pass_hash, status, role_id, COALESCE(totp_secret,''), totp_enabled,
		        failed_attempts, locked_until, created_at, updated_at
		 FROM ce_admin_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AdminAccount{}
	for rows.Next() {
		var a AdminAccount
		var totpSecret string
		var totpEnabled int
		if err := rows.Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Status, &a.RoleID, &totpSecret, &totpEnabled, &a.FailedAttempts, &a.LockedUntil, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.TOTPSecret = totpSecret
		a.TOTPEnabled = totpEnabled == 1
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (s *mysqlAdminStore) UpdateAccount(a *AdminAccount) error {
	a.UpdatedAt = time.Now()
	res, err := s.db.Exec(
		`UPDATE ce_admin_accounts SET username=?, pass_hash=?, status=?, role_id=?, totp_secret=NULLIF(?, ''), totp_enabled=?, failed_attempts=?, locked_until=?, updated_at=? WHERE id=?`,
		a.Username, a.PasswordHash, a.Status, a.RoleID, a.TOTPSecret, boolToInt(a.TOTPEnabled),
		a.FailedAttempts, a.LockedUntil, a.UpdatedAt, a.ID)
	if err != nil {
		if isDuplicate(err) {
			return ErrAdminExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAdminNotFound
	}
	return nil
}

func (s *mysqlAdminStore) CreateRole(r *Role) error {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_admin_roles (name, description, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		r.Name, r.Description, now, now)
	if err != nil {
		if isDuplicate(err) {
			return ErrRoleExists
		}
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	r.ID = id
	r.CreatedAt = now
	r.UpdatedAt = now
	return nil
}

func (s *mysqlAdminStore) GetRoleByName(name string) (*Role, error) {
	var r Role
	err := s.db.QueryRow(
		`SELECT id, name, description, created_at, updated_at FROM ce_admin_roles WHERE name = ?`, name).
		Scan(&r.ID, &r.Name, &r.Description, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrAdminNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *mysqlAdminStore) GetRoleByID(id int64) (*Role, error) {
	var r Role
	err := s.db.QueryRow(
		`SELECT id, name, description, created_at, updated_at FROM ce_admin_roles WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.Description, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrAdminNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *mysqlAdminStore) ListRoles() ([]*Role, error) {
	rows, err := s.db.Query(`SELECT id, name, description, created_at, updated_at FROM ce_admin_roles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Role{}
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *mysqlAdminStore) DeleteRole(id int64) error {
	// 先检查是否仍被管理员账户引用，避免外键冲突或孤儿引用。
	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ce_admin_accounts WHERE role_id = ?`, id).Scan(&cnt); err != nil {
		return err
	}
	if cnt > 0 {
		return ErrRoleInUse
	}
	// 先删权限关联，再删角色（事务）
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM ce_admin_role_perms WHERE role_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	res, err := tx.Exec(`DELETE FROM ce_admin_roles WHERE id = ?`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return ErrAdminNotFound
	}
	return tx.Commit()
}

// UpdateRole 按 ID 更新角色名与描述；改名时若与其它角色重名返回 ErrRoleExists。
func (s *mysqlAdminStore) UpdateRole(r *Role) error {
	if _, err := s.GetRoleByID(r.ID); err != nil {
		return err
	}
	if r.Name != "" {
		if other, err := s.GetRoleByName(r.Name); err == nil && other.ID != r.ID {
			return ErrRoleExists
		}
	}
	now := time.Now()
	res, err := s.db.Exec(
		`UPDATE ce_admin_roles SET name = ?, description = ?, updated_at = ? WHERE id = ?`,
		r.Name, r.Description, now, r.ID)
	if err != nil {
		if isDuplicate(err) {
			return ErrRoleExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAdminNotFound
	}
	return nil
}

func (s *mysqlAdminStore) SetRolePermissions(roleID int64, perms []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM ce_admin_role_perms WHERE role_id = ?`, roleID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, p := range perms {
		if _, err := tx.Exec(`INSERT INTO ce_admin_role_perms (role_id, perm_key) VALUES (?, ?)`, roleID, p); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *mysqlAdminStore) GetRolePermissions(roleID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT perm_key FROM ce_admin_role_perms WHERE role_id = ? ORDER BY perm_key`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
