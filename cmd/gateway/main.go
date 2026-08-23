package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

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

	r := buildRouter(cfg, log)

	addr := ":8080"
	log.Info("gateway starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}

// buildRouter 装配网关路由：边缘鉴权 + 业务服务反代 + 撮合服务只读收敛（§18.1）。
// 抽出以便单测验证「matching 写端点不直连网关」的资金安全不变量。
func buildRouter(cfg *config.Config, log *zap.Logger) *gin.Engine {
	r := gin.New()
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（若网关自身也位于 CDN/LB 之后，需在此填入其 IP/CIDR）。
	middleware.ConfigureTrustedProxies(r, cfg, log)
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
		// refresh/logout 走刷新令牌（body 中的 refresh_token）而非 Bearer access token；
		// 后端 user 服务将其列入公开组，故网关亦豁免，否则已登出（无 access token）的
		// 用户无法经网关刷新或登出，造成与后端策略不一致的 401（非漏洞，仅为策略对齐）。
		"/api/v1/user/refresh",
		"/api/v1/user/logout",
		"/api/v1/spot/depth",
		"/api/v1/spot/ws",
		"/api/v1/market/ticker",
		"/api/v1/market/ws",
		"/api/v1/market/depth",
		"/api/v1/market/trades",
		"/api/v1/market/klines",
		// 前端（crypto-exchange-web）兼容端点：单数 /kline 及其 WS 子路径 /kline/ws。
		// 遵循 AuthWithSkips 的「精确或 pre+"/" 前缀」匹配，故仅列 /api/v1/market/kline 即可同时豁免两者。
		"/api/v1/market/kline",
		// 管理后台（cmd/admin）用独立 admin token 鉴权，不走普通用户鉴权域；
		// 整段豁免后由 admin 后端自行校验 admin token（零信任：后端二次校验）。
		// 这样 web-admin 可经网关统一入口访问 /api/admin/*（含新增的 /api/admin/apikeys）。
		"/api/admin",
	))...)

	// 反向代理工厂（含 WebSocket 升级透传）。
	proxy := func(target string) gin.HandlerFunc {
		u, err := url.Parse(target)
		if err != nil {
			panic(err)
		}
		p := httputil.NewSingleHostReverseProxy(u)
		p.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, e error) {
			log.Error("proxy error", zap.String("target", target), zap.Error(e))
			rw.WriteHeader(http.StatusBadGateway)
		}
		return func(c *gin.Context) {
			// 包一层 writer：httputil.ReverseProxy 会向 writer 断言 http.CloseNotifier
			// （以及 websocket 升级时断言 http.Hijacker/http.Flusher）。gin 的 writer 在
			// 测试环境或新版 Go 下可能未实现 CloseNotifier，直接透传会 panic；proxyWriter
			// 安全补全这些接口（不支持时降级），既让测试可跑也避免线上潜在 panic。
			p.ServeHTTP(&proxyWriter{c.Writer}, c.Request)
		}
	}

	// 业务服务通用反代：把 /api/v1/<svc>/* 反代到对应后端。matching 不走此通用反代，
	// 其读写端点单独处理（见下），避免写端点被网关直连绕过 spot/futures 的资金安全层。
	for svc, target := range cfg.Services {
		if svc == "matching" {
			continue
		}
		path := "/api/v1/" + svc + "/*path"
		r.Any(path, proxy(target))
	}

	// 管理后台（cmd/admin）反代：admin 是独立鉴权域（admin token），不在 cfg.Services 内，
	// 故单独反代。目标地址由 admin.addr 推导（同机部署，演示/默认即 localhost:<port>）；
	// 仅当配置了 admin.addr 才启用，未配置则与之前一致（admin 仅直连 :8095）。
	// 边缘鉴权已在上方 AuthWithSkips 对 /api/admin 整段豁免，admin 后端再做 admin 鉴权。
	if a := cfg.Admin.Addr; a != "" {
		r.Any("/api/admin/*path", proxy(adminProxyTarget(a)))
	}

	// §18.1 网关层收敛：把撮合权威 cmd/matching 纳入网关统一入口，便于健康检查与
	// 行情/订单查询的统一寻址；但只暴露只读/行情端点（depth/ws/orders/trades/health）。
	// /order、/cancel、/match-now 等写端点刻意不代理——订单提交必须经 spot/futures，
	// 其内做资金预冻结（ledger.Freeze）与成交账本结算（ledger.Transfer），若经网关直连
	// cmd/matching 会绕过这套资金安全控制（cmd/matching 仅负责撮合，无钱包/账本）。
	if m := cfg.Services["matching"]; m != "" {
		mp := proxy(m)
		r.GET("/api/v1/matching/depth", mp)
		r.GET("/api/v1/matching/ws", mp)
		r.GET("/api/v1/matching/orders", mp)
		r.GET("/api/v1/matching/orders/:id", mp)
		r.GET("/api/v1/matching/trades", mp)
		r.GET("/api/v1/matching/health", mp)
	}

	return r
}

// adminProxyTarget 把 admin 监听地址（如 ":8095" 或 "127.0.0.1:8095"）推导为反代目标 URL。
// admin 监听地址通常不含 scheme，这里补 http://；同机演示部署直接补 localhost。
func adminProxyTarget(addr string) string {
	switch {
	case addr == "":
		return ""
	case strings.HasPrefix(addr, "http://"), strings.HasPrefix(addr, "https://"):
		return addr
	case strings.HasPrefix(addr, ":"):
		return "http://localhost" + addr
	default:
		return "http://" + addr
	}
}

// proxyWriter 包装 gin 的 ResponseWriter，安全补全 httputil.ReverseProxy 所需的接口。
// gin 的 writer 对 http.CloseNotifier 采用「断言后调用」实现，若底层 writer（如测试用的
// httptest.ResponseWriter、或新版 Go 中已移除 CloseNotify 的 *http.response）未实现该接口会
// 直接 panic；此处改为安全降级，并使 WebSocket 升级（Hijack/Flush）能正确透传到真实 writer。
type proxyWriter struct {
	gin.ResponseWriter
}

func (w *proxyWriter) CloseNotify() <-chan bool {
	// 不委托给 gin 的 CloseNotify：gin 的 CloseNotify 会以「断言后调用」方式访问底层
	// writer，若底层未实现 http.CloseNotifier（如测试用 httptest.ResponseWriter、或新版
	// Go 已移除该方法的 *http.response）会直接 panic。这里返回永不触发的 channel，等价于
	// 客户端未断开——代价是代理在客户端中途断开时不会取消上游请求（次要，不影响正确性）。
	return make(chan bool)
}

func (w *proxyWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *proxyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("proxyWriter: underlying writer does not support hijacking")
}
