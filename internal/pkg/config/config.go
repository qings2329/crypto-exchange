package config

import (
	"fmt"
	"net/http"
	"os"

	"github.com/coldlar/crypto-exchange/internal/oracle"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// Config 是全局配置结构，对应 configs/config.yaml。
type Config struct {
	Server struct {
		Port int `yaml:"port"`
		Mode string `yaml:"mode"`
		// RateLimitPerSec 单实例每 IP 限流阈值（默认 100 req/s）。
		RateLimitPerSec int `yaml:"rate_limit_per_sec"`
		// AllowedOrigins 允许的跨域来源白名单（为空则拒绝一切跨域）。
		AllowedOrigins []string `yaml:"allowed_origins"`
		// MaxBodyBytes 请求体大小上限（默认 1 MiB）。
		MaxBodyBytes int64 `yaml:"max_body_bytes"`
		// TLS 启用 HTTPS 所需的证书与私钥路径；同时配置才启用，否则明文 HTTP。
		TLS struct {
			CertFile string `yaml:"cert_file"`
			KeyFile  string `yaml:"key_file"`
		} `yaml:"tls"`
	} `yaml:"server"`

	MySQL struct {
		DSN     string `yaml:"dsn"`
		MaxOpen int    `yaml:"max_open"`
		MaxIdle int    `yaml:"max_idle"`
	} `yaml:"mysql"`

	Redis struct {
		Addr     string `yaml:"addr"`
		Password string `yaml:"password"`
		DB       int    `yaml:"db"`
	} `yaml:"redis"`

	Kafka struct {
		Brokers    []string `yaml:"brokers"`
		OrderTopic string   `yaml:"order_topic"`
		TradeTopic string   `yaml:"trade_topic"`
		DepthTopic string   `yaml:"depth_topic"`
		Version    string   `yaml:"version"` // 协议协商版本，如 "3.6.0"；空则使用内置默认（V3_6_0_0）
	} `yaml:"kafka"`

	// Auth 是 Bearer Token 共享密钥（HMAC-SHA256）。生产应从密钥管理注入，勿写死。
	Auth struct {
		Secret string `yaml:"secret"`
	} `yaml:"auth"`

	// Matching 是撮合引擎服务（cmd/matching）的配置。
	Matching struct {
		// URL 是 cmd/matching 服务的基址（如 http://127.0.0.1:8085）。
		// spot/futures 改为调用该服务的 HTTP(+WS) API，把匹配收敛为单一权威。
		URL string `yaml:"url"`
		// SnapshotIntervalSec 周期快照间隔（秒），<=0 时引擎默认 5s。
		SnapshotIntervalSec int `yaml:"snapshot_interval_sec"`
		// LeaderTTLSec leader 租约时长（秒），<=0 时默认 10s。
		LeaderTTLSec int `yaml:"leader_ttl_sec"`
		// Symbols 是 cmd/matching 预注册并提供服务的交易对集合；为空时取默认超集
		// （现货 BTC_USDT/ETH_USDT + 合约永续 BTC_USDT_PERP/ETH_USDT_PERP）。
		Symbols []string `yaml:"symbols"`
	} `yaml:"matching"`

	Services map[string]string `yaml:"services"`

	// Admin 是管理后台后端（cmd/admin）配置。
	Admin AdminConfig `yaml:"admin"`

	// Oracle 是指数价预言机配置（internal/oracle）。为空时各服务回退到内置演示喂价。
	Oracle oracle.OracleConf `yaml:"oracle"`
}

// AdminConfig 是管理后台后端（cmd/admin）配置。
type AdminConfig struct {
	// Addr 监听地址，如 ":8090"。
	Addr string `yaml:"addr"`
	// Username 是管理后台登录用户名。
	Username string `yaml:"username"`
	// PasswordHash 是登录密码的 bcrypt 哈希（口令比对使用 bcrypt.CompareHashAndPassword）。
	// 生产务必从密钥管理/环境变量（ADMIN_PASSWORD_HASH）注入，勿在文件中保留明文口令。
	PasswordHash string `yaml:"password_hash"`
	// Password 是遗留的明文口令回退（仅当 PasswordHash 为空时启用，并打告警日志）。
	// 新部署请改用 PasswordHash，不要再配置明文口令。
	Password string `yaml:"password"`
	// TokenTTLSec 签发的 admin token 有效期（秒），<=0 时默认 3600。
	TokenTTLSec int `yaml:"token_ttl_sec"`
	// AllowedOrigins 管理后台前端来源白名单（如 http://localhost:5174）。
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// Load 从给定路径读取 yaml 配置。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// 环境变量覆盖：生产环境从密钥管理/环境变量注入敏感项，避免把密钥写进配置文件。
	if v := os.Getenv("AUTH_SECRET"); v != "" {
		c.Auth.Secret = v
	}
	if v := os.Getenv("ADMIN_PASSWORD_HASH"); v != "" {
		c.Admin.PasswordHash = v
	}
	return &c, nil
}

// TLSEnabled 报告是否同时配置了证书与私钥（即应启用 HTTPS）。
func (c *Config) TLSEnabled() bool {
	return c.Server.TLS.CertFile != "" && c.Server.TLS.KeyFile != ""
}

// Listen 启动 HTTP(S) 服务：配置了 TLS 则启用 HTTPS，否则明文 HTTP。
// 供各 cmd 统一调用，避免散落的 r.Run / r.RunTLS 分支。
func (c *Config) Listen(r *gin.Engine, addr string) error {
	if c.TLSEnabled() {
		return r.RunTLS(addr, c.Server.TLS.CertFile, c.Server.TLS.KeyFile)
	}
	return r.Run(addr)
}

// ListenServer 与 Listen 等价，但基于已配置 ReadHeaderTimeout 等参数的 *http.Server
// （notification/risk 等服务使用），统一处理 TLS 分支。
func (c *Config) ListenServer(srv *http.Server) error {
	if c.TLSEnabled() {
		return srv.ListenAndServeTLS(c.Server.TLS.CertFile, c.Server.TLS.KeyFile)
	}
	return srv.ListenAndServe()
}
