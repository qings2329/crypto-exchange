package announcement

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // 注册 mysql 驱动（sql.Open("mysql", ...) 需要）

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// 公告模块迁移版本号（与用户模块 9101–9106、其它模块错开，避免 ce_schema_migrations 冲突）。
const announcementMigVer = 9401

// AnnouncementMigrations 是公告模块的建表迁移，运行时由 main 调 migrate.New(db, AnnouncementMigrations).Up()。
var AnnouncementMigrations = []migrate.Migration{
	{
		Version: announcementMigVer,
		Name:    "create_ce_announcements",
		Up: `CREATE TABLE IF NOT EXISTS ce_announcements (
    id           BIGINT       NOT NULL AUTO_INCREMENT,
    level        VARCHAR(16)  NOT NULL DEFAULT 'info',
    title        VARCHAR(128) NOT NULL,
    content      TEXT         NULL,
    active       TINYINT      NOT NULL DEFAULT 0,
    published_at DATETIME(3)  NULL,
    created_at   DATETIME(3)  NOT NULL,
    updated_at   DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_active_published (active, published_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_announcements;",
	},
}

// mysqlStore 是 Store 的 MySQL 实现。
type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 以 DSN 打开连接并应用公告迁移。与用户模块共用同一数据库
// （同一份 ce_schema_migrations），因此版本号必须全局唯一，互不重叠。
func NewMySQLStore(dsn string) (Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := migrate.New(db, AnnouncementMigrations).Up(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("announcement migrations: %w", err)
	}
	return &mysqlStore{db: db}, nil
}

func (s *mysqlStore) Create(a *Announcement) error {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO ce_announcements (level, title, content, active, published_at, created_at, updated_at)
		 VALUES (?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?)`,
		a.Level, a.Title, a.Content, boolToInt(a.Active), nullIfEmptyTime(a.PublishedAt), now, now)
	if err != nil {
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

func (s *mysqlStore) Update(a *Announcement) error {
	a.UpdatedAt = time.Now()
	res, err := s.db.Exec(
		`UPDATE ce_announcements SET level=?, title=?, content=NULLIF(?, ''), active=?, published_at=NULLIF(?, ''), updated_at=? WHERE id=?`,
		a.Level, a.Title, a.Content, boolToInt(a.Active), nullIfEmptyTime(a.PublishedAt), a.UpdatedAt, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mysqlStore) Delete(id int64) error {
	res, err := s.db.Exec(`DELETE FROM ce_announcements WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *mysqlStore) Get(id int64) (*Announcement, error) {
	var a Announcement
	var content string
	var publishedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, level, title, COALESCE(content,''), active, published_at, created_at, updated_at
		 FROM ce_announcements WHERE id=?`, id).
		Scan(&a.ID, &a.Level, &a.Title, &content, &a.Active, &publishedAt, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Content = content
	if publishedAt.Valid {
		a.PublishedAt = publishedAt.Time
	}
	return &a, nil
}

func (s *mysqlStore) ListAll() ([]*Announcement, error) {
	return s.queryAll(`SELECT id, level, title, COALESCE(content,''), active, published_at, created_at, updated_at
		FROM ce_announcements ORDER BY published_at DESC, id DESC`)
}

func (s *mysqlStore) ListActive() ([]*Announcement, error) {
	return s.queryAll(`SELECT id, level, title, COALESCE(content,''), active, published_at, created_at, updated_at
		FROM ce_announcements WHERE active=1 ORDER BY published_at DESC, id DESC`)
}

func (s *mysqlStore) queryAll(query string, args ...interface{}) ([]*Announcement, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Announcement{}
	for rows.Next() {
		var a Announcement
		var content string
		var publishedAt sql.NullTime
		if err := rows.Scan(&a.ID, &a.Level, &a.Title, &content, &a.Active, &publishedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Content = content
		if publishedAt.Valid {
			a.PublishedAt = publishedAt.Time
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ---- 辅助 ----

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullIfEmptyTime 把零值时间序列为 NULL，避免写入 0000-00-00 这类非法时间。
func nullIfEmptyTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}
