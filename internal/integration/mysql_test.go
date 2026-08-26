package integration

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/coldlar/crypto-exchange/internal/announcement"
	"github.com/coldlar/crypto-exchange/internal/apikeys"
	"github.com/coldlar/crypto-exchange/internal/bot"
	"github.com/coldlar/crypto-exchange/internal/copytrade"
	"github.com/coldlar/crypto-exchange/internal/earn"
	"github.com/coldlar/crypto-exchange/internal/lending"
	"github.com/coldlar/crypto-exchange/internal/margin"
	"github.com/coldlar/crypto-exchange/internal/matching/persist"
	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/options"
	"github.com/coldlar/crypto-exchange/internal/otc"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/referral"
	"github.com/coldlar/crypto-exchange/internal/risk"
	"github.com/coldlar/crypto-exchange/internal/services/user"
	"github.com/coldlar/crypto-exchange/internal/spot"
	"github.com/coldlar/crypto-exchange/internal/staking"
	"github.com/coldlar/crypto-exchange/internal/wealth"
)

// testDBName 是集成测试专用的隔离库名（建在「config.yaml 里的同一台 MySQL」上），
// 避免直接读写/迁移线上 wallet 库，也规避既有数据造成的迁移冲突（重复键、部分表状态）。
const testDBName = "wallet_it"

// mysqlBaseDSN 返回配置/环境变量里的真实 MySQL DSN。
func mysqlBaseDSN(t *testing.T) string {
	if d := os.Getenv("CE_MYSQL_DSN"); d != "" {
		return d
	}
	cfgPath := filepath.Join(repoRoot(t), "configs", "config.yaml")
	if cfg, err := config.Load(cfgPath); err == nil && cfg.MySQL.DSN != "" {
		return cfg.MySQL.DSN
	}
	return ""
}

// mysqlTestDSN 解析 base DSN，在「同一台 MySQL」上建隔离测试库 wallet_it 后返回其 DSN。
// 若当前账号无建库权限，则尝试使用显式指定的 CE_MYSQL_TEST_DSN（指向一个干净的测试库）；
// 两者皆不可用时，跳过测试——绝不在已存在数据/迁移漂移的线上库上直接迁移或读写。
func mysqlTestDSN(t *testing.T) string {
	base := mysqlBaseDSN(t)
	if base == "" {
		t.Skip("no MySQL DSN: set CE_MYSQL_DSN or configure mysql.dsn in configs/config.yaml")
	}
	cfg, err := mysql.ParseDSN(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// 1) 显式指定的干净测试库（最优先，避免对线上 MySQL 发起建库尝试）。
	if td := os.Getenv("CE_MYSQL_TEST_DSN"); td != "" {
		return td
	}
	// 2) 在配置里的同一台 MySQL 上自动建隔离测试库（需建库权限）。
	admin := *cfg
	admin.DBName = ""
	adm, aerr := sql.Open("mysql", admin.FormatDSN())
	if aerr == nil {
		_, cerr := adm.Exec("CREATE DATABASE IF NOT EXISTS " + testDBName)
		if cerr == nil {
			adm.Close()
			cfg.DBName = testDBName
			t.Logf("using isolated test database %q on configured MySQL", testDBName)
			return cfg.FormatDSN()
		}
		t.Logf("cannot auto-create isolated test db (%v); will try CE_MYSQL_TEST_DSN", cerr)
		adm.Close()
	}
	t.Skip("MySQL integration test needs a dedicated clean test database: " +
		"auto-create is denied on the configured MySQL and CE_MYSQL_TEST_DSN is unset. " +
		"Set CE_MYSQL_TEST_DSN to a clean test DB DSN (same MySQL server as configs/config.yaml), " +
		"or grant CREATE DATABASE to the configured account.")
	return ""
}

// repoRoot 由本测试源文件向上找到 go.mod 所在目录（仓库根），保证无论从哪个目录跑
// `go test` 都能定位 configs/config.yaml。
func repoRoot(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root (go.mod) not found")
	return ""
}

// openMySQL 打开并探活真实 MySQL；无 DSN 或不可达时跳过（不破坏无 MySQL 环境的 `go test ./...`）。
func openMySQL(t *testing.T) *sql.DB {
	dsn := mysqlTestDSN(t)
	if dsn == "" {
		t.Skip("no MySQL DSN: set CE_MYSQL_DSN or configure mysql.dsn in configs/config.yaml")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(5 * time.Second)
	if err := db.Ping(); err != nil {
		db.Close()
		t.Skipf("MySQL unreachable (skipping real-DB integration): %v", err)
	}
	return db
}

// TestMySQLStoresMigrate 在真实 MySQL 上逐个执行各模块的 Up 迁移（建表/加列），
// 覆盖此前仅在内存模式被跳过的 MySQL 存储路径。只校验迁移成功，不写入业务数据。
func TestMySQLStoresMigrate(t *testing.T) {
	db := openMySQL(t)
	defer db.Close()
	dsn := mysqlTestDSN(t)

	type smoke struct {
		name string
		fn   func() error
	}
	cases := []smoke{
		{"announcement", func() error { _, e := announcement.NewMySQLStore(dsn); return e }},
		{"apikeys", func() error { _, e := apikeys.NewMySQLStore(dsn); return e }},
		{"bot", func() error { _, e := bot.NewMySQLStore(dsn); return e }},
		{"copytrade", func() error { _, e := copytrade.NewMySQLStore(dsn); return e }},
		{"earn", func() error { _, e := earn.NewMySQLStore(dsn); return e }},
		{"lending", func() error { _, e := lending.NewMySQLStore(dsn); return e }},
		{"margin", func() error { _, e := margin.NewMySQLStore(dsn); return e }},
		{"otc", func() error { _, e := otc.NewMySQLStore(dsn); return e }},
		{"options", func() error { _, e := options.NewMySQLStore(dsn); return e }},
		{"referral", func() error { _, e := referral.NewMySQLStore(dsn); return e }},
		{"staking", func() error { _, e := staking.NewMySQLStore(dsn); return e }},
		{"user", func() error { _, e := user.NewMySQLStore(dsn); return e }},
		{"spot", func() error { _, e := spot.NewMySQLStore(dsn); return e }},
		{"wealth", func() error { _, e := wealth.NewMySQLStore(dsn); return e }},
		{"matching/persist", func() error { _, e := persist.NewMySQLStore(dsn); return e }},
		// notification / risk 接收 *sql.DB 而非 dsn。
		{"notification", func() error { _, e := notification.NewMySQLStore(db); return e }},
		{"risk", func() error { _, e := risk.NewMySQLStore(db); return e }},
	}
	for _, c := range cases {
		if err := c.fn(); err != nil {
			t.Errorf("migrate %s: %v", c.name, err)
		}
	}
}

// TestMySQLNotificationRoundTrip 验证通知模块在真实 MySQL 上的 发布→列表→已读→删除 全链路。
func TestMySQLNotificationRoundTrip(t *testing.T) {
	db := openMySQL(t)
	defer db.Close()
	store, err := notification.NewMySQLStore(db)
	if err != nil {
		t.Fatalf("notification store: %v", err)
	}
	svc := notification.New(store)
	const uid = 990001
	n, err := svc.Publish(notification.PublishInput{
		UserID: uid, Type: notification.TypeSystem, Title: "IT round-trip", Body: "hello",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer svc.Delete(uid, n.ID)

	list, err := svc.List(uid, false, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == n.ID {
			found = true
			if x.Title != "IT round-trip" {
				t.Fatalf("title mismatch: %q", x.Title)
			}
		}
	}
	if !found {
		t.Fatalf("published notification not found in list")
	}

	if err := svc.MarkRead(uid, n.ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	cnt, err := svc.UnreadCount(uid)
	if err != nil {
		t.Fatalf("unread count: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 unread after mark-read, got %d", cnt)
	}
}

// TestMySQLAnnouncementRoundTrip 验证公告模块在真实 MySQL 上的 创建→列表→删除 全链路。
func TestMySQLAnnouncementRoundTrip(t *testing.T) {
	db := openMySQL(t)
	defer db.Close()
	dsn := mysqlTestDSN(t)
	store, err := announcement.NewMySQLStore(dsn)
	if err != nil {
		t.Fatalf("announcement store: %v", err)
	}
	svc := announcement.NewService(store)

	lvl, title, content, active := "info", "IT announce", "body", true
	a, err := svc.Create(announcement.AnnouncementInput{
		Level:   &lvl,
		Title:   &title,
		Content: &content,
		Active:  &active,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() {
		if derr := svc.Delete(a.ID); derr != nil && !errors.Is(derr, announcement.ErrNotFound) {
			t.Logf("cleanup delete: %v", derr)
		}
	}()

	list, err := svc.ListActive()
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created announcement not found in ListActive")
	}
}
