package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
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
