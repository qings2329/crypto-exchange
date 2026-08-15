package redis

import (
	"testing"
	"time"
)

// TestMemLimiterAllowsUpToLimit 验证内存固定窗口：window 内允许 limit 次、第 limit+1 次拒绝。
func TestMemLimiterAllowsUpToLimit(t *testing.T) {
	lim := New("", "", 0) // 空 addr -> 内存实现
	const limit = 3
	window := time.Second
	key := "ip:1.2.3.4"
	var allowed int
	for i := 0; i < limit; i++ {
		if lim.Allow(key, limit, window) {
			allowed++
		}
	}
	if allowed != limit {
		t.Fatalf("expected %d allowed, got %d", limit, allowed)
	}
	if lim.Allow(key, limit, window) {
		t.Fatal("expected deny on limit+1")
	}
}

// TestMemLimiterWindowReset 验证窗口过期后计数重置、重新放行。
func TestMemLimiterWindowReset(t *testing.T) {
	lim := New("", "", 0)
	window := 20 * time.Millisecond
	key := "ip:9.9.9.9"
	if !lim.Allow(key, 1, window) {
		t.Fatal("first should allow")
	}
	if lim.Allow(key, 1, window) {
		t.Fatal("second within window should deny")
	}
	time.Sleep(window + 5*time.Millisecond)
	if !lim.Allow(key, 1, window) {
		t.Fatal("after window reset should allow again")
	}
}

// TestNewEmptyAddrReturnsMem 验证空 addr 返回内存实现（不尝试连接 Redis）。
func TestNewEmptyAddrReturnsMem(t *testing.T) {
	lim := New("", "", 0)
	if _, ok := lim.(*memLimiter); !ok {
		t.Fatalf("expected *memLimiter for empty addr, got %T", lim)
	}
}
