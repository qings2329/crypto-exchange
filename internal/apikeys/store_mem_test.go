package apikeys

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateAndParseKey(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !strings.HasPrefix(kp.Key, keyPrefixHeader) {
		t.Fatalf("key prefix missing: %q", kp.Key)
	}
	prefix, secret, ok := ParseKey(kp.Key)
	if !ok || prefix != kp.Prefix || secret != kp.Secret {
		t.Fatalf("ParseKey round-trip failed: ok=%v prefix=%q secret=%q", ok, prefix, secret)
	}
	if HashKey(kp.Key) != kp.Key[:0]+HashKey(kp.Key) {
		t.Fatal("sanity")
	}
	// 不同 Key 哈希应不同
	kp2, _ := GenerateKey()
	if HashKey(kp.Key) == HashKey(kp2.Key) {
		t.Fatal("expected distinct hashes for distinct keys")
	}
}

func TestMemStoreLifecycle(t *testing.T) {
	s := NewMemStore()
	kp, _ := GenerateKey()
	rec := &APIKey{
		UserID:      42,
		Label:       "quant-bot",
		Prefix:      kp.Prefix,
		KeyHash:     HashKey(kp.Key),
		Permissions: []string{"read:market", "trade:spot"},
		CreatedBy:   7,
		CreatedAt:   time.Now(),
	}
	if err := s.Create(rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.ID == 0 {
		t.Fatal("expected assigned ID")
	}
	if rec.Status != StatusActive {
		t.Fatalf("expected active status, got %q", rec.Status)
	}

	// 列表
	all, _ := s.List(ListFilter{})
	if len(all) != 1 {
		t.Fatalf("expected 1 key, got %d", len(all))
	}
	byUser, _ := s.List(ListFilter{UserID: 42})
	if len(byUser) != 1 {
		t.Fatalf("expected 1 key for user 42, got %d", len(byUser))
	}
	byOther, _ := s.List(ListFilter{UserID: 99})
	if len(byOther) != 0 {
		t.Fatalf("expected 0 keys for user 99, got %d", len(byOther))
	}

	// GetByID / GetByKeyHash（内部模型保留 key_hash；对外视图 View() 已剔除该字段）。
	got, err := s.GetByID(rec.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.View().Prefix != kp.Prefix {
		t.Fatalf("view prefix mismatch: %q", got.View().Prefix)
	}
	byHash, err := s.GetByKeyHash(HashKey(kp.Key))
	if err != nil || byHash.ID != rec.ID {
		t.Fatalf("GetByKeyHash: err=%v id=%d", err, byHash.ID)
	}
	if _, err := s.GetByKeyHash("nope"); err != ErrKeyNotFound {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}

	// 吊销
	if err := s.Revoke(rec.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	rev, _ := s.GetByID(rec.ID)
	if rev.Status != StatusRevoked || rev.RevokedAt == nil {
		t.Fatalf("expected revoked status with RevokedAt, got %+v", rev)
	}
	// 重复吊销 → ErrKeyRevoked
	if err := s.Revoke(rec.ID); err != ErrKeyRevoked {
		t.Fatalf("expected ErrKeyRevoked, got %v", err)
	}

	// 非法输入
	if err := s.Create(&APIKey{}); err != ErrInvalidInput {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}
