package settlement

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// HSMConfig 是生产 HSM/KMS 签名后端的连接配置（位于 HotWalletConfig.HSM）。敏感字段
// （endpoint 含访问凭据、public_key）经环境变量注入，不写进 configs/config.yaml。
//
// 设计：真实安全模块（AWS KMS / Vault Transit / 硬件 HSM via PKCS#11）通常由交易所前置一个
// 「内部签名服务」封装，对外暴露一个对 32 字节摘要做 ECDSA 签名的 HTTPS 端点（私钥永不离开
// 安全域）。本仓库实现该路径的通用适配器 remote-http（纯标准库、无额外依赖），生产只需把
// endpoint 指向你的签名服务即可；其它后端（aws-kms / pkcs11）实现同一 KeySigner 接口后，
// 用 RegisterExternalSigner 注入（见 NewExternalKeySigner）。
type HSMConfig struct {
	// Kind 后端类型：当前支持 "remote-http"（内部签名服务）。其它类型需自行实现 KeySigner 后注入。
	Kind string `yaml:"kind"`
	// Endpoint 是签名服务「对摘要签名」的 HTTPS 端点（如 https://hsm.internal/sign）。
	// 服务须对传入的 32 字节 digest 直接做 ECDSA 签名（不再二次哈希），返回 {r,s}(hex) 或
	// {signature: DER-hex}，与 ParseExternalDERSignature 兼容。
	Endpoint string `yaml:"endpoint"`
	// APIKey 可选：作为 Bearer Token 注入 Authorization 头（内部服务鉴权）。
	APIKey string `yaml:"api_key"`
	// PublicKey 是安全模块导出的 secp256k1 公钥（hex，压缩/非压缩均可）。用于派生地址、
	// 推导 ETH recovery id、校验签名；必须与后端密钥对应（设备可导出，不涉密）。
	PublicKey string `yaml:"public_key"`
}

// parsePubKeyHex 解析 hex 公钥（压缩 33 字节或 非压缩 65 字节）为 secp256k1 公钥。
func parsePubKeyHex(s string) (*secp256k1.PublicKey, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return nil, fmt.Errorf("公钥 hex 非法: %w", err)
	}
	pub, err := secp256k1.ParsePubKey(b)
	if err != nil {
		return nil, fmt.Errorf("公钥解析失败（需 33/65 字节）: %w", err)
	}
	return pub, nil
}

// parseHexBig 把 hex 整数字符串（可选 0x 前缀）解析为 *big.Int。
func parseHexBig(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return nil, fmt.Errorf("空 hex 整数")
	}
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return nil, fmt.Errorf("hex 整数解析失败: %q", s)
	}
	return v, nil
}

// remoteHSMKeySigner 是生产 HSM/KMS 的真实 KeySigner 适配：把 SignDigest 转成对内部签名服务
// 的 HTTPS 调用，私钥永不离开安全模块。这是「离线签名边界」接入真实安全模块的生产路径之一
// （与软件后端共用同一 realSigner，其余 settlement 代码不变）。
type remoteHSMKeySigner struct {
	pub      *secp256k1.PublicKey
	endpoint string
	apiKey   string
	client   *http.Client
}

// NewRemoteHSMKeySigner 构造指向内部签名服务的 HSM 后端适配。
func NewRemoteHSMKeySigner(pub *secp256k1.PublicKey, endpoint, apiKey string) (KeySigner, error) {
	if pub == nil {
		return nil, fmt.Errorf("remote HSM 签名器需要设备导出的公钥")
	}
	if endpoint == "" {
		return nil, fmt.Errorf("remote HSM 签名器需要 endpoint")
	}
	return &remoteHSMKeySigner{
		pub:      pub,
		endpoint: endpoint,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// SignDigest 把 32 字节摘要 POST 到签名服务，解析返回的 (r,s) 或 DER 签名。
// 签名服务须对 digest 直接做 ECDSA（prehashed），与 HSM/KMS 的「签名预哈希」语义一致。
func (k *remoteHSMKeySigner) SignDigest(ctx context.Context, digest [32]byte) (*big.Int, *big.Int, error) {
	reqBody, err := json.Marshal(map[string]string{"digest": hex.EncodeToString(digest[:])})
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+k.apiKey)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("remote HSM 签名请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("remote HSM 签名返回 %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		R         string `json:"r"`
		S         string `json:"s"`
		Signature string `json:"signature"` // DER hex（AWS KMS / Vault 常见返回）
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, fmt.Errorf("remote HSM 响应解析失败: %w", err)
	}
	if out.Signature != "" {
		// DER 编码（如 AWS KMS Sign / Vault transit / PKCS#11）经 ParseExternalDERSignature 解出 (r,s)。
		return ParseExternalDERSignature(mustHex(out.Signature))
	}
	if out.R != "" && out.S != "" {
		r, err := parseHexBig(out.R)
		if err != nil {
			return nil, nil, err
		}
		s, err := parseHexBig(out.S)
		if err != nil {
			return nil, nil, err
		}
		return r, s, nil
	}
	return nil, nil, fmt.Errorf("remote HSM 响应缺少 r/s 或 signature")
}

func (k *remoteHSMKeySigner) Public() *secp256k1.PublicKey { return k.pub }

// newHSMKeySigner 按 HSMConfig 构建生产签名后端；供 SignerBackend="external" 在配置齐全时
// 自动注册到全局注册表（无需部署方手写 RegisterExternalSigner 调用）。
func newHSMKeySigner(conf HotWalletConfig) (KeySigner, error) {
	h := conf.HSM
	if h.Kind == "" {
		return nil, fmt.Errorf("hsm.kind 为空，无法自动注册外部签名后端（可改用 RegisterExternalSigner 手动注入）")
	}
	switch h.Kind {
	case "remote-http":
		pub, err := parsePubKeyHex(h.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("hsm.public_key 非法: %w", err)
		}
		return NewRemoteHSMKeySigner(pub, h.Endpoint, h.APIKey)
	default:
		return nil, fmt.Errorf("暂不支持的 hsm.kind=%q（当前仅 remote-http；aws-kms/pkcs11 需实现 KeySigner 后用 RegisterExternalSigner 注入）", h.Kind)
	}
}
