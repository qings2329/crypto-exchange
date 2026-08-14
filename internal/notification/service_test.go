package notification

import "testing"

func newTestSvc() *Service {
	return New(NewMemStore())
}

func TestPublishAndList(t *testing.T) {
	svc := newTestSvc()
	n, err := svc.Publish(PublishInput{UserID: 1, Type: TypeKYCAproved, Title: "KYC 通过", Body: "已认证"})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n.ID == 0 || n.Status != StatusUnread {
		t.Fatalf("unexpected notification: %+v", n)
	}

	ns, err := svc.List(1, false, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ns) != 1 {
		t.Fatalf("want 1 got %d", len(ns))
	}

	cnt, _ := svc.UnreadCount(1)
	if cnt != 1 {
		t.Fatalf("want 1 unread got %d", cnt)
	}
}

func TestPublishValidation(t *testing.T) {
	svc := newTestSvc()
	if _, err := svc.Publish(PublishInput{UserID: 0, Title: "x"}); err == nil {
		t.Fatal("want error for invalid user_id")
	}
	if _, err := svc.Publish(PublishInput{UserID: 1}); err == nil {
		t.Fatal("want error for empty title")
	}
}

func TestUnknownTypeDefaultsToSystem(t *testing.T) {
	svc := newTestSvc()
	n, err := svc.Publish(PublishInput{UserID: 2, Type: "weird_type", Title: "t"})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n.Type != TypeSystem {
		t.Fatalf("want system got %s", n.Type)
	}
}

func TestMarkReadAndAll(t *testing.T) {
	svc := newTestSvc()
	svc.Publish(PublishInput{UserID: 3, Title: "a"})
	svc.Publish(PublishInput{UserID: 3, Title: "b"})

	if err := svc.MarkRead(3, 999); err != ErrNotFound {
		t.Fatalf("want ErrNotFound got %v", err)
	}
	// 标记第一条已读
	ns, _ := svc.List(3, false, 0)
	if err := svc.MarkRead(3, ns[0].ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	cnt, _ := svc.UnreadCount(3)
	if cnt != 1 {
		t.Fatalf("want 1 unread got %d", cnt)
	}
	// 仅未读过滤
	un, _ := svc.List(3, true, 0)
	if len(un) != 1 {
		t.Fatalf("want 1 unread in filter got %d", len(un))
	}
	// 全部已读
	marked, err := svc.MarkAllRead(3)
	if err != nil || marked != 1 {
		t.Fatalf("mark all: marked=%d err=%v", marked, err)
	}
	cnt, _ = svc.UnreadCount(3)
	if cnt != 0 {
		t.Fatalf("want 0 unread got %d", cnt)
	}
}

func TestListAll(t *testing.T) {
	svc := newTestSvc()
	svc.Publish(PublishInput{UserID: 10, Title: "a"})
	svc.Publish(PublishInput{UserID: 11, Title: "b"})
	all, err := svc.ListAll(0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 got %d", len(all))
	}
}
