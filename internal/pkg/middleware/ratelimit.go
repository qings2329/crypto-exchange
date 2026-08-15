package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/redis"
)

// limiter 是简单的固定窗口限流器（生产建议用 redis 令牌桶做分布式限流）。
type limiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	counts    map[string]*windowCount
}

type windowCount struct {
	count    int
	resetAt  time.Time
}

// RateLimit 按客户端 IP 限流：每 window 内最多允许 limit 次请求。
func RateLimit(limit int, window time.Duration) gin.HandlerFunc {
	l := &limiter{
		limit:  limit,
		window: window,
		counts: make(map[string]*windowCount),
	}
	return func(c *gin.Context) {
		key := c.ClientIP()
		l.mu.Lock()
		wc, ok := l.counts[key]
		now := time.Now()
		if !ok || now.After(wc.resetAt) {
			wc = &windowCount{count: 0, resetAt: now.Add(window)}
			l.counts[key] = wc
		}
		wc.count++
		exceeded := wc.count > limit
		l.mu.Unlock()

		if exceeded {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    429,
				"message": "rate limit exceeded",
			})
			return
		}
		c.Next()
	}
}

// RateLimitWith 与 RateLimit 语义一致，但计数后端由外部 RateLimiter 提供（如 Redis 分布式限流）。
// 用于把所有服务的限流收敛为集群级（多实例共享计数），无 Redis 时 redis.New 自动回退内存。
func RateLimitWith(lim redis.RateLimiter, limit int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !lim.Allow(c.ClientIP(), limit, window) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    429,
				"message": "rate limit exceeded",
			})
			return
		}
		c.Next()
	}
}
