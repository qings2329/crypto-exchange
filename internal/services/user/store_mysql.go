package user

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// isDuplicate 判断是否为 MySQL 唯一键冲突（错误号 1062）。
func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1062
	}
	return false
}

// 用户模块迁移版本号（与 ledger 的 9001/9002 错开，避免 ce_schema_migrations 冲突）。
const (
	userMigVerUsers   = 9101
	userMigVerCodes   = 9102
	userMigVerRefresh = 9103
	userMigVerKYC     = 9104
)

// UserMigrations 是用户模块的建表迁移，运行时由 main 调 migrate.New(db, UserMigrations).Up()。
var UserMigrations = []migrate.Migration{
	{
		Version: userMigVerUsers,
		Name:    "create_ce_users",
		Up: `CREATE TABLE IF NOT EXISTS ce_users (
    id            BIGINT        NOT NULL AUTO_INCREMENT,
    email         VARCHAR(191)  NOT NULL DEFAULT '',
    phone         VARCHAR(32)   NOT NULL DEFAULT '',
    pass_hash     VARCHAR(255)  NOT NULL,
    status        TINYINT       NOT NULL DEFAULT 0,
    kyc_level     TINYINT       NOT NULL DEFAULT 0,
    tfa_secret    VARCHAR(255)  NULL,
    tfa_enabled   TINYINT       NOT NULL DEFAULT 0,
    email_verified TINYINT      NOT NULL DEFAULT 0,
    phone_verified TINYINT      NOT NULL DEFAULT 0,
    created_at    DATETIME(3)   NOT NULL,
    updated_at    DATETIME(3)   NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_email (email),
    UNIQUE KEY uk_phone (phone)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_users;",
	},
	{
		Version: userMigVerCodes,
		Name:    "create_ce_user_codes",
		Up: `CREATE TABLE IF NOT EXISTS ce_user_codes (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    user_id    BIGINT       NOT NULL DEFAULT 0,
    target     VARCHAR(191) NOT NULL,
    purpose    VARCHAR(32)  NOT NULL,
    code       VARCHAR(32)  NOT NULL,
    expires_at DATETIME(3)  NOT NULL,
    consumed   TINYINT      NOT NULL DEFAULT 0,
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_target_purpose (target, purpose)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_user_codes;",
	},
	{
		Version: userMigVerRefresh,
		Name:    "create_ce_user_refresh",
		Up: `CREATE TABLE IF NOT EXISTS ce_user_refresh (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    user_id    BIGINT       NOT NULL,
    token_hash VARCHAR(255) NOT NULL,
    expires_at DATETIME(3)  NOT NULL,
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_token_hash (token_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_user_refresh;",
	},
	{
		Version: userMigVerKYC,
		Name:    "create_ce_user_kyc",
		Up: `CREATE TABLE IF NOT EXISTS ce_user_kyc (
    user_id       BIGINT       NOT NULL,
    real_name     VARCHAR(128) NOT NULL,
    id_type       VARCHAR(32)  NOT NULL,
    id_number     VARCHAR(128) NOT NULL,
    doc_front     VARCHAR(512) NULL,
    doc_back      VARCHAR(512) NULL,
    status        TINYINT      NOT NULL DEFAULT 1,
    reject_reason VARCHAR(255) NULL,
    submitted_at  DATETIME(3)  NOT NULL,
    reviewed_at   DATETIME(3)  NULL,
    reviewer      VARCHAR(128) NULL,
    PRIMARY KEY (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_user_kyc;",
	},
}

// mysqlStore 是 Store 的 MySQL 实现。
type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 以 DSN 打开连接并应用迁移。
func NewMySQLStore(dsn string) (Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := migrate.New(db, UserMigrations).Up(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("user migrations: %w", err)
	}
	return &mysqlStore{db: db}, nil
}

func (s *mysqlStore) CreateUser(u *User) error {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_users (email, phone, pass_hash, status, kyc_level, tfa_secret, tfa_enabled, email_verified, phone_verified, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`,
		nullIfEmpty(u.Email), nullIfEmpty(u.Phone), u.PassHash, int(u.Status), int(u.KYCLevel),
		u.TFASecret, boolToInt(u.TFAEnabled), boolToInt(u.EmailVerified), boolToInt(u.PhoneVerified),
		now, now)
	if err != nil {
		if isDuplicate(err) {
			return ErrUserExists
		}
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	u.ID = id
	u.CreatedAt = now
	u.UpdatedAt = now
	return nil
}

func (s *mysqlStore) GetByEmail(email string) (*User, error) {
	return s.scanUser(`SELECT id, email, phone, pass_hash, status, kyc_level, COALESCE(tfa_secret,''), tfa_enabled, email_verified, phone_verified, created_at, updated_at FROM ce_users WHERE email = ?`, email)
}

func (s *mysqlStore) GetByPhone(phone string) (*User, error) {
	return s.scanUser(`SELECT id, email, phone, pass_hash, status, kyc_level, COALESCE(tfa_secret,''), tfa_enabled, email_verified, phone_verified, created_at, updated_at FROM ce_users WHERE phone = ?`, phone)
}

func (s *mysqlStore) GetByID(id int64) (*User, error) {
	return s.scanUser(`SELECT id, email, phone, pass_hash, status, kyc_level, COALESCE(tfa_secret,''), tfa_enabled, email_verified, phone_verified, created_at, updated_at FROM ce_users WHERE id = ?`, id)
}

func (s *mysqlStore) scanUser(query string, args ...interface{}) (*User, error) {
	row := s.db.QueryRow(query, args...)
	return s.scanUserRow(row)
}

func (s *mysqlStore) scanUserRow(row scanner) (*User, error) {
	var u User
	var email, phone, tfaSecret sql.NullString
	var tfaEnabled, emailVerified, phoneVerified int
	err := row.Scan(
		&u.ID, &email, &phone, &u.PassHash, &u.Status, &u.KYCLevel,
		&tfaSecret, &tfaEnabled, &emailVerified, &phoneVerified, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Email = email.String
	u.Phone = phone.String
	u.TFASecret = tfaSecret.String
	u.TFAEnabled = tfaEnabled == 1
	u.EmailVerified = emailVerified == 1
	u.PhoneVerified = phoneVerified == 1
	return &u, nil
}

func (s *mysqlStore) scanUserFromRows(rows *sql.Rows) (*User, error) {
	return s.scanUserRow(rows)
}

// scanner 兼容 *sql.Row 与 *sql.Rows 的 Scan 接口。
type scanner interface {
	Scan(dest ...interface{}) error
}

func (s *mysqlStore) UpdateUser(u *User) error {
	u.UpdatedAt = time.Now()
	res, err := s.db.Exec(
		`UPDATE ce_users SET email=NULLIF(?,''), phone=NULLIF(?,''), pass_hash=?, status=?, kyc_level=?,
		 tfa_secret=NULLIF(?,''), tfa_enabled=?, email_verified=?, phone_verified=?, updated_at=? WHERE id=?`,
		u.Email, u.Phone, u.PassHash, int(u.Status), int(u.KYCLevel),
		u.TFASecret, boolToInt(u.TFAEnabled), boolToInt(u.EmailVerified), boolToInt(u.PhoneVerified),
		u.UpdatedAt, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mysqlStore) ListAll() ([]*User, error) {
	rows, err := s.db.Query(
		`SELECT id, email, phone, pass_hash, status, kyc_level, COALESCE(tfa_secret,''), tfa_enabled, email_verified, phone_verified, created_at, updated_at FROM ce_users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u, err := s.scanUserFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *mysqlStore) SaveCode(c *VerifyCode) error {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_user_codes (user_id, target, purpose, code, expires_at, consumed, created_at) VALUES (?, ?, ?, ?, ?, 0, ?)`,
		c.UserID, c.Target, c.Purpose, c.Code, c.ExpiresAt, now)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	c.ID = id
	c.CreatedAt = now
	return nil
}

func (s *mysqlStore) GetLatestCode(target, purpose string) (*VerifyCode, error) {
	var c VerifyCode
	err := s.db.QueryRow(
		`SELECT id, user_id, target, purpose, code, expires_at, consumed, created_at FROM ce_user_codes WHERE target=? AND purpose=? ORDER BY id DESC LIMIT 1`,
		target, purpose).Scan(&c.ID, &c.UserID, &c.Target, &c.Purpose, &c.Code, &c.ExpiresAt, &c.Consumed, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *mysqlStore) ConsumeCode(id int64) error {
	res, err := s.db.Exec(`UPDATE ce_user_codes SET consumed=1 WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mysqlStore) SaveRefresh(rt *RefreshToken) error {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_user_refresh (user_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		rt.UserID, rt.TokenHash, rt.ExpiresAt, now)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	rt.ID = id
	rt.CreatedAt = now
	return nil
}

func (s *mysqlStore) GetRefresh(tokenHash string) (*RefreshToken, error) {
	var rt RefreshToken
	err := s.db.QueryRow(
		`SELECT id, user_id, token_hash, expires_at, created_at FROM ce_user_refresh WHERE token_hash=?`,
		tokenHash).Scan(&rt.ID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt, &rt.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rt, nil
}

func (s *mysqlStore) DeleteRefresh(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM ce_user_refresh WHERE token_hash=?`, tokenHash)
	return err
}

func (s *mysqlStore) DeleteUserRefreshes(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM ce_user_refresh WHERE user_id=?`, userID)
	return err
}

func (s *mysqlStore) SaveKYC(k *KYCSubmission) error {
	_, err := s.db.Exec(
		`INSERT INTO ce_user_kyc (user_id, real_name, id_type, id_number, doc_front, doc_back, status, reject_reason, submitted_at, reviewed_at, reviewer)
		 VALUES (?, ?, ?, ?, NULLIF(?,''), NULLIF(?,''), ?, NULLIF(?,''), ?, NULL, NULL)
		 ON DUPLICATE KEY UPDATE real_name=VALUES(real_name), id_type=VALUES(id_type), id_number=VALUES(id_number),
		 doc_front=VALUES(doc_front), doc_back=VALUES(doc_back), status=VALUES(status), reject_reason=VALUES(reject_reason),
		 submitted_at=VALUES(submitted_at), reviewed_at=NULL, reviewer=NULL`,
		k.UserID, k.RealName, k.IDType, k.IDNumber, k.DocFront, k.DocBack, int(k.Status), k.RejectReason, time.Now())
	return err
}

func (s *mysqlStore) GetKYC(userID int64) (*KYCSubmission, error) {
	var k KYCSubmission
	var docFront, docBack, rejectReason, reviewer sql.NullString
	var reviewedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT user_id, real_name, id_type, id_number, doc_front, doc_back, status, reject_reason, submitted_at, reviewed_at, reviewer
		 FROM ce_user_kyc WHERE user_id=?`, userID).
		Scan(&k.UserID, &k.RealName, &k.IDType, &k.IDNumber, &docFront, &docBack, &k.Status, &rejectReason, &k.SubmittedAt, &reviewedAt, &reviewer)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.DocFront = docFront.String
	k.DocBack = docBack.String
	k.RejectReason = rejectReason.String
	k.Reviewer = reviewer.String
	if reviewedAt.Valid {
		k.ReviewedAt = reviewedAt.Time
	}
	return &k, nil
}

func (s *mysqlStore) UpdateKYC(k *KYCSubmission) error {
	_, err := s.db.Exec(
		`UPDATE ce_user_kyc SET status=?, reject_reason=NULLIF(?,''), reviewed_at=?, reviewer=NULLIF(?,''), doc_front=NULLIF(?,''), doc_back=NULLIF(?,'') WHERE user_id=?`,
		int(k.Status), k.RejectReason, k.ReviewedAt, k.Reviewer, k.DocFront, k.DocBack, k.UserID)
	if err != nil {
		return err
	}
	return nil
}

// ---- 辅助 ----

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
