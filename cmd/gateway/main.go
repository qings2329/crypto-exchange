package main

import (
	"flag"
	"net/http/httputil"
	"net/url"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// 网关：统一鉴权 + 限流 + 反向代理到后端微服务。
func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	r := gin.New()
	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)
	// 边缘鉴权：放行认证与公开行情端点，其余一律强制鉴权；后端服务再做二次校验（零信任）。
	// Auth 追加在 Common 安全套件之后，确保所有响应（含 401）都带安全头、且被拒绝请求也被审计。
	// 注意：公开行情（market/spot 的 ticker/depth/ws）与注册相关端点必须豁免，否则前端无法免登录消费行情。
	mws := middleware.Common(log, cfg)
	r.Use(append(mws, middleware.AuthWithSkips(verifier,
		"/api/v1/user/login",
		"/api/v1/user/register",
		"/api/v1/user/send-code",
		"/api/v1/user/verify",
		"/api/v1/user/forgot",
		"/api/v1/user/reset",
		"/api/v1/spot/depth",
		"/api/v1/spot/ws",
		"/api/v1/market/ticker",
		"/api/v1/market/ws",
		"/api/v1/market/depth",
		"/api/v1/market/trades",
		"/api/v1/market/klines",
	))...)

	// 简单路由转发：把各业务线路径反代到对应后端服务（含 WebSocket 升级透传）。
	proxy := func(target string) gin.HandlerFunc {
		u, err := url.Parse(target)
		if err != nil {
			panic(err)
		}
		p := httputil.NewSingleHostReverseProxy(u)
		return func(c *gin.Context) {
			p.ServeHTTP(c.Writer, c.Request)
		}
	}

	for svc, target := range cfg.Services {
		path := "/api/v1/" + svc + "/*path"
		r.Any(path, proxy(target))
	}

	addr := ":8080"
	log.Info("gateway starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
