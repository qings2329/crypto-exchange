package middleware

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

// ConfigureTrustedProxies 设置 gin 的受信任代理并立即打印生效状态，便于运维从启动
// 日志确认配置是否加载。解析失败直接 fatal（错误地址会导致 IP 归因不符合预期）。
// 配置留空表示不信任任何代理，c.ClientIP() 退化为直连 RemoteAddr（服务直连公网的安全默认）。
func ConfigureTrustedProxies(r *gin.Engine, cfg *config.Config, log *zap.Logger) {
	if log == nil {
		log = zap.NewNop()
	}
	if err := r.SetTrustedProxies(cfg.Server.TrustedProxies); err != nil {
		log.Fatal("set trusted proxies", zap.Error(err))
	}
	LogTrustedProxies(cfg, log)
}

// LogTrustedProxies 打印受信任代理生效状态。
func LogTrustedProxies(cfg *config.Config, log *zap.Logger) {
	if log == nil {
		return
	}
	if len(cfg.Server.TrustedProxies) == 0 {
		log.Warn("server.trusted_proxies is EMPTY: not trusting any proxy; c.ClientIP() uses direct RemoteAddr. " +
			"Set it if this service runs behind a gateway/LB/CDN, otherwise all client IPs collapse to the upstream IP.")
		return
	}
	log.Info("server.trusted_proxies configured; c.ClientIP() reads real client IP from X-Forwarded-For",
		zap.Strings("trusted_proxies", cfg.Server.TrustedProxies))
}
