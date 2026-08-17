package migrate

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// TestRunMigrations 是迁移运行器的集成测试，由 MYSQL_TEST_DSN 门控（指向一个测试库）。
func TestRunMigrations(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping migration integration test")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	migs := []Migration{
		{Version: 1, Name: "m1", Up: "CREATE TABLE IF NOT EXISTS ce_migrate_demo (id INT PRIMARY KEY)", Down: "DROP TABLE IF EXISTS ce_migrate_demo"},
		{Version: 2, Name: "m2", Up: "ALTER TABLE ce_migrate_demo ADD COLUMN v VARCHAR(32)", Down: "ALTER TABLE ce_migrate_demo DROP COLUMN v"},
	}

	r := New(db, migs)
	// 先清掉可能残留的版本记录，保证测试可重入（远程库状态跨运行保留）。
	_ = r.Down(-1)
	// 第一次 Up：应用 1、2。
	if err := r.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	applied, err := r.appliedVersions(context.Background(), db)
	if err != nil {
		t.Fatalf("appliedVersions: %v", err)
	}
	if !applied[1] || !applied[2] {
		t.Fatalf("expected versions 1,2 applied, got %v", applied)
	}
	// 第二次 Up：幂等，不应报错。
	if err := r.Up(); err != nil {
		t.Fatalf("up idempotent: %v", err)
	}
	// 回滚到版本 1（保留 m1，撤销 m2）。
	if err := r.Down(1); err != nil {
		t.Fatalf("down to 1: %v", err)
	}
	applied, _ = r.appliedVersions(context.Background(), db)
	if applied[2] {
		t.Fatal("version 2 should be rolled back")
	}
	if !applied[1] {
		t.Fatal("version 1 should remain")
	}
	// 清理
	_ = r.Down(-1)
}
