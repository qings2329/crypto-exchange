package announcement

import "testing"

func newTestService() *Service {
	return NewService(NewMemStore())
}

func ptr(s string) *string { return &s }
func pbool(b bool) *bool   { return &b }

func TestCreate(t *testing.T) {
	svc := newTestService()
	a, err := svc.Create(AnnouncementInput{
		Level:   ptr(LevelWarning),
		Title:   ptr("维护公告"),
		Content: ptr("今晚升级"),
		Active:  pbool(true),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ID == 0 {
		t.Fatal("expected non-zero id")
	}
	if a.Level != LevelWarning || a.Title != "维护公告" {
		t.Fatalf("unexpected fields: %+v", a)
	}
	if !a.Active {
		t.Fatal("expected active")
	}
	if a.PublishedAt.IsZero() {
		t.Fatal("expected published_at to be set when active")
	}
}

func TestCreateDefaultsLevel(t *testing.T) {
	svc := newTestService()
	a, err := svc.Create(AnnouncementInput{Title: ptr("普通公告")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Level != LevelInfo {
		t.Fatalf("expected default level info, got %s", a.Level)
	}
}

func TestCreateInvalidLevel(t *testing.T) {
	svc := newTestService()
	if _, err := svc.Create(AnnouncementInput{Level: ptr("critical"), Title: ptr("x")}); err != ErrInvalidLevel {
		t.Fatalf("expected ErrInvalidLevel, got %v", err)
	}
}

func TestCreateTitleRequired(t *testing.T) {
	svc := newTestService()
	if _, err := svc.Create(AnnouncementInput{Level: ptr(LevelInfo)}); err != ErrTitleRequired {
		t.Fatalf("expected ErrTitleRequired, got %v", err)
	}
}

func TestCreateTitleTooLong(t *testing.T) {
	svc := newTestService()
	long := make([]rune, maxTitleLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := svc.Create(AnnouncementInput{Title: ptr(string(long))}); err != ErrTitleTooLong {
		t.Fatalf("expected ErrTitleTooLong, got %v", err)
	}
}

func TestListActive(t *testing.T) {
	svc := newTestService()
	active, err := svc.Create(AnnouncementInput{Title: ptr("已发布"), Active: pbool(true)})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	if _, err := svc.Create(AnnouncementInput{Title: ptr("草稿"), Active: pbool(false)}); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	active2, err := svc.Create(AnnouncementInput{Title: ptr("又一条已发布"), Active: pbool(true)})
	if err != nil {
		t.Fatalf("create active2: %v", err)
	}

	list, err := svc.ListActive()
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 active, got %d", len(list))
	}
	// 按发布时间倒序：后发布（active2）应排在前面。
	if list[0].ID != active2.ID || list[1].ID != active.ID {
		t.Fatalf("unexpected order: %+v", list)
	}
}

func TestListAll(t *testing.T) {
	svc := newTestService()
	if _, err := svc.Create(AnnouncementInput{Title: ptr("a"), Active: pbool(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(AnnouncementInput{Title: ptr("b"), Active: pbool(false)}); err != nil {
		t.Fatal(err)
	}
	all, err := svc.ListAll()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 total, got %d", len(all))
	}
}

func TestUpdate(t *testing.T) {
	svc := newTestService()
	a, err := svc.Create(AnnouncementInput{Title: ptr("原"), Active: pbool(false)})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(a.ID, AnnouncementInput{
		Title:  ptr("改"),
		Active: pbool(true),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Title != "改" || !updated.Active {
		t.Fatalf("unexpected update: %+v", updated)
	}
	// 切到发布态后应自动填充发布时间。
	if updated.PublishedAt.IsZero() {
		t.Fatal("expected published_at set after activating")
	}
	// 原等级保留。
	if updated.Level != LevelInfo {
		t.Fatalf("expected level preserved, got %s", updated.Level)
	}
}

func TestUpdateInvalidLevel(t *testing.T) {
	svc := newTestService()
	a, err := svc.Create(AnnouncementInput{Title: ptr("x")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(a.ID, AnnouncementInput{Level: ptr("bad")}); err != ErrInvalidLevel {
		t.Fatalf("expected ErrInvalidLevel, got %v", err)
	}
}

func TestUpdateNotFound(t *testing.T) {
	svc := newTestService()
	if _, err := svc.Update(999, AnnouncementInput{Title: ptr("x")}); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	svc := newTestService()
	a, err := svc.Create(AnnouncementInput{Title: ptr("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(a.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestDeleteNotFound(t *testing.T) {
	svc := newTestService()
	if err := svc.Delete(42); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
