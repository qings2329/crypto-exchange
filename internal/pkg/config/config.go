package config

import (
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/coldlar/crypto-exchange/internal/oracle"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// Config 是全局配置结构，对应 configs/config.yaml。
type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Mode string `yaml:"mode"`
		// RateLimitPerSec 单实例每 IP 限流阈值（默认 100 req/s）。
		RateLimitPerSec int `yaml:"rate_limit_per_sec"`
		// AllowedOrigins 允许的跨域来源白名单（为空则拒绝一切跨域）。
		AllowedOrigins []string `yaml:"allowed_origins"`
		// MaxBodyBytes 请求体大小上限（默认 1 MiB）。
		MaxBodyBytes int64 `yaml:"max_body_bytes"`
		// TrustedProxies 受信任的反向代理/负载均衡 IP 或 CIDR；配置后 c.ClientIP()
		// 从 X-Forwarded-For 取真实客户端 IP（用于登录 IP 限流、审计 IP、全局限流正确归因）。
		// 留空则不信任任何代理，直接使用直连对端 IP（RemoteAddr）。
		TrustedProxies []string `yaml:"trusted_proxies"`
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

	// Settlement 是清算/结算服务（cmd/settlement）配置。
	Settlement struct {
		// TradeFeeRate 是交易所对 taker 收取的交易手续费率（如 0.001=0.1%）。
		// <=0 时使用 internal/settlement.DefaultTradeFeeRate。
		TradeFeeRate float64 `yaml:"trade_fee_rate"`
		// ChainRPC 是链上提现网关（T-03 链上 RPC 半边）配置：生产填真实节点 RPC，
		// 网关据此广播提现并取回真实 TxHash；留空/未启用则回退离线模拟网关
		// （MockWithdrawGateway），保证无外部节点也能运行（fail-degraded）。
		ChainRPC settlement.ChainRPCConfig `yaml:"chain_rpc"`
	} `yaml:"settlement"`

	// InfluxDB 是行情 K 线持久化配置（T-16）：已收盘 K 线写入时序库，
	// 供行情服务在内存环形缓冲（klineCap=500）之外回取更长历史。
	// URL 为空时行情服务仅用内存，不连接 InfluxDB（fail-degraded）。
	InfluxDB struct {
		URL    string `yaml:"url"`    // 如 http://127.0.0.1:8086
		Token  string `yaml:"token"`  // 有读写权限的 API token
		Org    string `yaml:"org"`    // 组织名
		Bucket string `yaml:"bucket"` // 目标 bucket（如 market）
	} `yaml:"influxdb"`

	// ES 是成交检索配置（T-16）：每笔成交索引入 Elasticsearch，支持历史成交检索
	// （symbol / 买卖方向 / 时间窗）。URL 为空时行情服务仅用内存，不连接 ES（fail-degraded）。
	ES struct {
		URL   string `yaml:"url"`   // 如 http://127.0.0.1:9200
		Index string `yaml:"index"` // 成交索引名；空则用默认 "trades"
	} `yaml:"es"`

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
	// MaxLoginFailures 连续登录失败达到该次数后锁定账户（防暴力破解），<=0 时默认 5。
	MaxLoginFailures int `yaml:"max_login_failures"`
	// LoginLockoutSec 账户锁定持续时间（秒，自动过期），<=0 时默认 900（15 分钟）。
	LoginLockoutSec int `yaml:"login_lockout_sec"`
	// LoginRateLimitPerIP 单 IP 在 LoginRateWindowSec 窗口内的登录尝试上限（基于 IP 的限流，
	// 防单 IP 自动化爆破，并缓解账户级锁定的 DoS 取舍），<=0 时默认 10。
	LoginRateLimitPerIP int `yaml:"login_rate_limit_per_ip"`
	// LoginRateWindowSec 单 IP 登录限流窗口（秒），<=0 时默认 60。
	LoginRateWindowSec int `yaml:"login_rate_window_sec"`
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
	// 链上 RPC：真实节点 URL（常含 API key）与离线签名私钥属敏感信息，生产从环境变量注入，
	// 不写进 configs/config.yaml（与 AUTH_SECRET 同一模式）。未设置时沿用 YAML 默认值。
	if v := os.Getenv("CHAIN_RPC_ENABLED"); v != "" {
		c.Settlement.ChainRPC.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CHAIN_RPC_ENDPOINT_ETH"); v != "" {
		if c.Settlement.ChainRPC.Endpoints == nil {
			c.Settlement.ChainRPC.Endpoints = map[string]string{}
		}
		c.Settlement.ChainRPC.Endpoints["ETH"] = v
	}
	if v := os.Getenv("CHAIN_RPC_ENDPOINT_BTC"); v != "" {
		if c.Settlement.ChainRPC.Endpoints == nil {
			c.Settlement.ChainRPC.Endpoints = map[string]string{}
		}
		c.Settlement.ChainRPC.Endpoints["BTC"] = v
	}
	if v := os.Getenv("CHAIN_RPC_ENDPOINT_TRON"); v != "" {
		if c.Settlement.ChainRPC.Endpoints == nil {
			c.Settlement.ChainRPC.Endpoints = map[string]string{}
		}
		c.Settlement.ChainRPC.Endpoints["TRON"] = v
	}
	if v := os.Getenv("CHAIN_RPC_REQUIRED_CONFIRMATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Settlement.ChainRPC.Required = n
		}
	}
	if v := os.Getenv("CHAIN_RPC_POLL_INTERVAL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Settlement.ChainRPC.PollSec = n
		}
	}
	// 离线签名边界（热钱包）：私钥与签名后端 keyID 同样从环境变量注入，不落配置。
	if v := os.Getenv("HOT_WALLET_ENABLED"); v != "" {
		c.Settlement.ChainRPC.HotWallet.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("HOT_WALLET_SIGNER_TYPE"); v != "" {
		c.Settlement.ChainRPC.HotWallet.SignerType = v
	}
	if v := os.Getenv("HOT_WALLET_SIGNER_BACKEND"); v != "" {
		c.Settlement.ChainRPC.HotWallet.SignerBackend = v
	}
	if v := os.Getenv("HOT_WALLET_SIGNER_KEY"); v != "" {
		c.Settlement.ChainRPC.HotWallet.SignerKey = v
	}
	if v := os.Getenv("HOT_WALLET_ETH_CHAIN_ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			c.Settlement.ChainRPC.HotWallet.EthChainID = n
		}
	}
	if v := os.Getenv("HOT_WALLET_ETH_GAS_PRICE_WEI"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			c.Settlement.ChainRPC.HotWallet.EthGasPriceWei = n
		}
	}
	if v := os.Getenv("HOT_WALLET_ETH_GAS_LIMIT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			c.Settlement.ChainRPC.HotWallet.EthGasLimit = n
		}
	}
	// 生产 HSM/KMS 后端连接配置（SignerBackend="external" 时按此自动构造真实签名后端）：
	// endpoint 常含访问凭据、public_key 是设备导出公钥，均属敏感信息，从环境变量注入，
	// 不写进 configs/config.yaml（与 AUTH_SECRET 同一模式）。未设置时沿用 YAML 默认值。
	if v := os.Getenv("HSM_KIND"); v != "" {
		c.Settlement.ChainRPC.HotWallet.HSM.Kind = v
	}
	if v := os.Getenv("HSM_ENDPOINT"); v != "" {
		c.Settlement.ChainRPC.HotWallet.HSM.Endpoint = v
	}
	if v := os.Getenv("HSM_API_KEY"); v != "" {
		c.Settlement.ChainRPC.HotWallet.HSM.APIKey = v
	}
	if v := os.Getenv("HSM_PUBLIC_KEY"); v != "" {
		c.Settlement.ChainRPC.HotWallet.HSM.PublicKey = v
	}
	// 充值地址 HD 派生用的账户级 xpub（HSM 内由 xprv 派生后导出）。属敏感信息，从环境变量
	// 注入，不写进 configs/config.yaml（与 HSM_PUBLIC_KEY 同一模式）。未设置时沿用 YAML 默认值。
	if v := os.Getenv("DEPOSIT_XPUB"); v != "" {
		c.Settlement.ChainRPC.HotWallet.Deposit.XPUB = v
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
