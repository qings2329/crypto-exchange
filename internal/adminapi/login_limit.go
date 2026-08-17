package adminapi

import (
	"sync"
	"time"
)

// loginIPMaxTracked 是进程内跟踪的 IP 桶上限，用于有界内存。
// 超过上限后对新 IP 优雅降级（直接放行），避免被海量不同 IP 打爆内存；
// 该极端分布式场景由账户级锁定兜底。
const loginIPMaxTracked = 10000

// loginIPLimiter 对管理后台登录接口做基于来源 IP 的定窗限流：
//   - 防单 IP 自动化爆破（高频试密）；
//   - 缓解「账户级锁定」可能被用于锁定真实管理员的 DoS 取舍（限流按来源 IP，
//     不波及使用其它 IP 的合法管理员）。
//
// 限流状态为进程内内存，适用于单实例 admin 服务；多副本部署需配合可信代理 +
// 共享存储（如 redis）以跨实例一致。IP 取自 c.ClientIP()（受 cfg.Server.TrustedProxies 影响）。
type loginIPLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	buckets map[string]*loginIPBucket
}

type loginIPBucket struct {
	start time.Time
	count int
}

// newLoginIPLimiter 构造限流器；limit/窗口 <=0 时取默认（10 次 / 60 秒）。
func newLoginIPLimiter(limit int, window time.Duration) *loginIPLimiter {
	if limit <= 0 {
		limit = 10
	}
	if window <= 0 {
		window = time.Minute
	}
	return &loginIPLimiter{
		window:  window,
		limit:   limit,
		buckets: make(map[string]*loginIPBucket),
	}
}

// allow 记录一次登录尝试并判断是否放行。窗口内达到上限则返回 false（拒绝）。
// 已跟踪的 IP 始终受控；仅「新 IP」受跟踪上限约束，超限则降级放行（内存有界）。
func (l *loginIPLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if b, ok := l.buckets[ip]; ok {
		if now.Sub(b.start) > l.window {
			// 窗口已过期：重置桶。
			l.buckets[ip] = &loginIPBucket{start: now, count: 1}
			return true
		}
		if b.count >= l.limit {
			return false
		}
		b.count++
		return true
	}
	// 新 IP：跟踪上限内才记录，否则优雅降级放行。
	if len(l.buckets) >= loginIPMaxTracked {
		return true
	}
	l.buckets[ip] = &loginIPBucket{start: now, count: 1}
	return true
}
