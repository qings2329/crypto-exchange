package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

// SecurityHeaders 注入安全响应头并隐藏 Server 标识，降低指纹与点击劫持等风险。
// 不依赖任何外部中间件，纯本地实现。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		// 去除 gin 默认 Server 头，避免暴露框架版本。
		h.Set("Server", "")
		c.Next()
	}
}

// MaxBodySize 限制请求体大小，防御大 payload 造成的资源耗尽（DoS）。
func MaxBodySize(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		// ContentLength 已知且超限时快速拒绝（chunked 为 -1，跳过预判，由 MaxBytesReader 兜底）。
		if c.Request.ContentLength > maxBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"code":    413,
				"message": "request body too large",
			})
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// CORS 受控跨域：仅允许显式配置的 origins；未在白名单的 origin 不回显，浏览器将拒绝。
// allowedOrigins 为空时即默认拒绝一切跨域请求（最安全默认）。
func CORS(allowedOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if _, ok := allowed[origin]; ok {
				h := c.Writer.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				if c.Request.Method == http.MethodOptions {
					c.AbortWithStatus(http.StatusNoContent)
					return
				}
			}
			// 不在白名单：不设置 ACAO，跨域请求被浏览器拦截。
		}
		c.Next()
	}
}

// Audit 访问审计日志：方法、路径、用户、状态码、耗时、客户端 IP。
// 供安全审计与异常行为追溯使用，不依赖外部存储（落地到结构化日志，可由日志系统采集）。
func Audit(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		uid, _ := UserID(c)
		log.Info("access",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
			zap.String("client_ip", c.ClientIP()),
			zap.Int64("user_id", uid),
		)
	}
}

// AuthWithSkips 在 Auth 基础上，对以指定前缀开头的路径放行（用于公开端点，
// 如行情查询、健康检查、登录注册）。其余路径仍强制鉴权。
func AuthWithSkips(v *TokenVerifier, skipPrefixes ...string) gin.HandlerFunc {
	auth := Auth(v)
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		for _, pre := range skipPrefixes {
			if strings.HasPrefix(p, pre) {
				c.Next()
				return
			}
		}
		auth(c)
	}
}

// Common 返回一组标准安全中间件（不含鉴权，鉴权由各服务内部或入口自行叠加），
// 便于所有服务以一致顺序接入。顺序：Recovery → 审计 → 限流 → 安全头 → CORS → 请求体限制。
//
// 设计要点：Audit 紧跟 Recovery 注册，在 c.Next() 之后记录最终状态码，可覆盖所有响应
// （含被限流/被鉴权拒绝的请求）；SecurityHeaders/CORS/MaxBodySize 均注册在入口追加的
// Auth 之前，因此即便 Auth 拒绝(Abort)也能为响应写入安全头。限流与请求体上限取自配置，
// 缺省分别 100 req/s 与 1 MiB；多实例下为单实例内存限流（暂不引入 Redis 的前提下的基础防护）。
func Common(log *zap.Logger, cfg *config.Config) []gin.HandlerFunc {
	limit := cfg.Server.RateLimitPerSec
	if limit <= 0 {
		limit = 100
	}
	maxBody := cfg.Server.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 1 << 20 // 1 MiB
	}
	return []gin.HandlerFunc{
		gin.Recovery(),
		Audit(log),
		RateLimit(limit, time.Second),
		SecurityHeaders(),
		CORS(cfg.Server.AllowedOrigins),
		MaxBodySize(maxBody),
	}
}
