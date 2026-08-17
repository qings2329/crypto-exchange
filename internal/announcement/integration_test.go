package announcement

import (
	"os"
	"testing"
)

// TestMySQLRoundTrip 是公告模块的 MySQL 集成测试：仅在设置环境变量 MYSQL_TEST_DSN 时运行，
// 指向一个测试库。NewMySQLStore 会自动建表（迁移 9401）并验证 CRUD / 列表的 SQL 往返。
// 无 MySQL 环境自动跳过，不阻塞普通单元测试（与 internal/ledger 的 MYSQL_TEST_DSN 约定一致）。
//
// 运行方式（需本地/CI 有可用 MySQL）：
//
//	MYSQL_TEST_DSN="user:pass@tcp(127.0.0.1:3306)/ce_test?parseTime=true" \
//	  go test ./internal/announcement/... -run TestMySQLRoundTrip -v
func TestMySQLRoundTrip(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping announcement MySQL integration test")
	}
	store, err := NewMySQLStore(dsn)
	if err != nil {
		t.Fatalf("NewMySQLStore: %v", err)
	}
	svc := NewService(store)

	// 1) 创建草稿（active=false，published_at 应为 0 值）。
	draft, err := svc.Create(AnnouncementInput{Title: ptr("it-draft")})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	defer svc.Delete(draft.ID)

	// 2) 创建已发布（active=true，published_at 自动填充；level 显式 maintenance）。
	pub, err := svc.Create(AnnouncementInput{
		Level: ptr(LevelMaintenance), Title: ptr("it-pub"), Content: ptr("c"), Active: pbool(true),
	})
	if err != nil {
		t.Fatalf("create pub: %v", err)
	}
	defer svc.Delete(pub.ID)

	// 3) Get 往返：草稿 inactive + published_at 零值；已发布 active + 已填充 + level 正确。
	gotDraft, err := svc.Get(draft.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if gotDraft.Active || !gotDraft.PublishedAt.IsZero() {
		t.Fatalf("draft should be inactive with zero published_at: %+v", gotDraft)
	}
	gotPub, err := svc.Get(pub.ID)
	if err != nil {
		t.Fatalf("get pub: %v", err)
	}
	if !gotPub.Active || gotPub.PublishedAt.IsZero() || gotPub.Level != LevelMaintenance {
		t.Fatalf("pub mismatch: %+v", gotPub)
	}

	// 4) ListActive 仅含已发布，且包含刚发布的这条。
	active, err := svc.ListActive()
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	for _, a := range active {
		if !a.Active {
			t.Fatalf("ListActive returned inactive: %+v", a)
		}
	}
	found := false
	for _, a := range active {
		if a.ID == pub.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("published announcement missing from ListActive")
	}

	// 5) 部分更新：草稿改为已发布，published_at 应自动填充，其它字段保留。
	if _, err := svc.Update(draft.ID, AnnouncementInput{Active: pbool(true)}); err != nil {
		t.Fatalf("update: %v", err)
	}
	gotDraft2, _ := svc.Get(draft.ID)
	if !gotDraft2.Active || gotDraft2.PublishedAt.IsZero() {
		t.Fatalf("after activate expected active + published_at: %+v", gotDraft2)
	}

	// 6) 删除后不可见。
	if err := svc.Delete(pub.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(pub.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

// TestMySQLMigrationIdempotent 验证迁移 9401 可重复运行（Up 幂等），
// 与用户模块共用同一 ce_schema_migrations 不冲突。仅在 MYSQL_TEST_DSN 设置时运行。
func TestMySQLMigrationIdempotent(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping announcement MySQL integration test")
	}
	// 第一次建表
	if _, err := NewMySQLStore(dsn); err != nil {
		t.Fatalf("first NewMySQLStore: %v", err)
	}
	// 第二次再建（应幂等，不报错）
	if _, err := NewMySQLStore(dsn); err != nil {
		t.Fatalf("second NewMySQLStore (idempotent) should not error: %v", err)
	}
}
