// Package redis 提供 Redis 客户端封装与分布式限流（令牌桶/固定窗口）能力，用于把
// docker-compose 中已声明但未实际使用的 Redis 接入业务（T-16）。
//
// 设计：RateLimiter 接口抽象「是否放行一次请求」；New 按配置返回两种实现——
//   - addr 非空：redisLimiter，经 Lua 脚本在 Redis 内做原子固定窗口计数（INCR + PEXPIRE），
//     多网关/多服务实例共享同一计数，实现集群级限流；Redis 不可达时降级到内存限流，
//     保证限流在故障期间仍然生效（fail-degraded，而非 fail-open 放行）。
//   - addr 为空：memLimiter，纯内存固定窗口（本地开发/无 Redis 时与原有行为一致）。
package redis

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter 是分布式限流抽象：Allow 返回本次请求是否放行。
type RateLimiter interface {
	// Allow 按 key（通常为客户 IP）在 window 窗口内是否仍允许第 limit 次以内的请求。
	// 返回 true 表示放行，false 表示超出限额。
	Allow(key string, limit int, window time.Duration) bool
	// Close 释放底层连接（内存实现为 no-op）。
	Close() error
}

// --- 内存固定窗口限流（回退 / 无 Redis 时使用） ---

type memLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	counts map[string]*windowCount
}

type windowCount struct {
	count   int
	resetAt time.Time
}

func newMemLimiter(limit int, window time.Duration) *memLimiter {
	return &memLimiter{limit: limit, window: window, counts: map[string]*windowCount{}}
}

func (m *memLimiter) Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return false
	}
	now := time.Now()
	m.mu.Lock()
	wc, ok := m.counts[key]
	if !ok || now.After(wc.resetAt) {
		wc = &windowCount{count: 0, resetAt: now.Add(window)}
		m.counts[key] = wc
	}
	wc.count++
	exceeded := wc.count > limit
	m.mu.Unlock()
	return !exceeded
}

func (m *memLimiter) Close() error { return nil }

// --- Redis 固定窗口限流（Lua 原子计数） ---

// rateScript 在 Redis 内原子地完成「递增+按需设置过期」的固定窗口计数：
// KEYS[1]=限流键，ARGV[1]=限额，ARGV[2]=窗口毫秒。返回当前窗口内累计计数。
var rateScript = redis.NewScript(`
local cur = redis.call('incr', KEYS[1])
if cur == 1 then
  redis.call('pexpire', KEYS[1], tonumber(ARGV[2]))
end
return cur
`)

type redisLimiter struct {
	client  *redis.Client
	fallback *memLimiter
}

// New 按配置构造限流实现：addr 为空返回内存实现；否则返回 Redis 实现（内置内存降级）。
func New(addr, password string, db int) RateLimiter {
	if addr == "" {
		return newMemLimiter(0, time.Second) // limit 在使用处由 Allow 参数决定
	}
	client := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})
	return &redisLimiter{client: client, fallback: newMemLimiter(0, time.Second)}
}

func (r *redisLimiter) Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return false
	}
	ctx := context.Background()
	res, err := rateScript.Run(ctx, r.client, []string{"ratelimit:" + key}, limit, window.Milliseconds()).Int64()
	if err != nil {
		// Redis 不可达：降级到内存限流，保证限流仍生效（fail-degraded）。
		return r.fallback.Allow(key, limit, window)
	}
	return res <= int64(limit)
}

func (r *redisLimiter) Close() error {
	return r.client.Close()
}
