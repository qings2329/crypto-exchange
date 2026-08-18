package settlement

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/tyler-smith/go-bip32"
)

// DepositConfig 是「给用户生成充值地址」的配置（配置驱动，敏感 xpub 经环境变量注入，不落 YAML）。
// 设计遵循本模块 HSM 模型「进程只持公钥、私钥留 HSM」：
//   - xpub 是**账户/外部链级**扩展公钥（m/44'/coin'/0'/0，BIP44 硬化层级由 HSM 内 xprv 派生后导出），
//     进程用它做**非硬化**子派生 index=userID，得到每用户独立的真实地址，全程无需任何私钥。
//   - 未配置（Enabled=false 或 xpub 为空）→ GenerateAddress 回退确定性 mock 占位地址（fail-degraded）。
type DepositConfig struct {
	// Enabled 是否启用 HD 充值地址派生（false → 回退 mock）。
	Enabled bool `yaml:"enabled"`
	// XPUB 账户/外部链级扩展公钥（base58，HSM 导出）。为空视为未配置。
	XPUB string `yaml:"xpub"`
	// BTCAddressType BTC 地址类型："p2wpkh"（默认，bech32 bc1…）或 "p2pkh"（base58 1…）。
	BTCAddressType string `yaml:"btc_address_type"`
}

// DepositAddressGenerator 按 userID 从配置 xpub 非硬化派生每用户真实充值地址（ETH/BTC/TRON）。
// 仅持公钥，符合「进程不持有私钥」的 HSM 模型。
type DepositAddressGenerator struct {
	mu      sync.Mutex  // 保护 master 的并发非硬化派生（go-bip32 的 Key 非线程安全）
	master  *bip32.Key // 公钥（IsPrivate=false），派生边界在外部链级。
	btcType string
}

// NewDepositAddressGenerator 解析 xpub 构造生成器；xpub 非法/为空返回错误（→ 调用方 fail-degraded）。
func NewDepositAddressGenerator(conf DepositConfig) (*DepositAddressGenerator, error) {
	if conf.XPUB == "" {
		return nil, fmt.Errorf("deposit xpub 为空（未配置 HD 充值地址）")
	}
	master, err := bip32.B58Deserialize(conf.XPUB)
	if err != nil {
		return nil, fmt.Errorf("deposit xpub 解析失败: %w", err)
	}
	if master.IsPrivate {
		return nil, fmt.Errorf("deposit xpub 必须是公钥扩展密钥（不能含私钥）")
	}
	btcType := conf.BTCAddressType
	if btcType == "" {
		btcType = "p2wpkh"
	}
	return &DepositAddressGenerator{master: master, btcType: btcType}, nil
}

// Address 为用户派生充值地址：非硬化 Child(userID) → 子公钥 → 按链编码。
// userID 必须是合法 BIP32 非硬化索引（0 ≤ userID ≤ 2^31-1）；越界或派生失败返回 error，
// 由 GenerateAddress 回退 mock（fail-degraded）。
func (g *DepositAddressGenerator) Address(userID int64, chain Chain) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if userID < 0 || userID > math.MaxInt32 {
		return "", fmt.Errorf("userID %d 超出 BIP32 非硬化索引范围 [0, %d]", userID, math.MaxInt32)
	}
	child, err := g.master.NewChildKey(uint32(userID))
	if err != nil {
		return "", fmt.Errorf("派生用户 %d 子地址失败: %w", userID, err)
	}
	pub, err := secp256k1.ParsePubKey(child.Key)
	if err != nil {
		return "", fmt.Errorf("子公钥解析失败: %w", err)
	}
	switch chain {
	case ChainETH:
		return deriveETHAddress(pub), nil
	case ChainTRON:
		return deriveTronAddress(pub), nil
	case ChainBTC:
		if g.btcType == "p2pkh" {
			return deriveP2PKHAddress(pub), nil
		}
		return deriveP2WPKHAddress(pub), nil
	case ChainSOL:
		// Solana 用 Ed25519，无法从 secp256k1 xpub 派生，按 userID 确定性派生（见 solana.go）。
		return deriveSolanaAddress(userID), nil
	default:
		return "", fmt.Errorf("充值地址派生不支持链 %s", chain)
	}
}

// —— 全局注册表（镜像 HSM 的 RegisterExternalSigner / lookupExternalSigner 模式）——
// GenerateAddress 命中此生成器时返回真实地址，否则回退 mock。避免改动 Mock 网关构造签名。

var depositAddrGen atomic.Pointer[DepositAddressGenerator]

// SetDepositAddressGenerator 注入（或清空，传 nil）HD 充值地址生成器。测试与生产装配均可用。
func SetDepositAddressGenerator(g *DepositAddressGenerator) {
	depositAddrGen.Store(g)
}

// ConfigureDepositAddresses 按配置装配生成器：Enabled 且 xpub 非空时构建并注册；构建失败则
// 留空（不 panic），由 GenerateAddress 回退 mock（fail-degraded）。
func ConfigureDepositAddresses(conf DepositConfig) {
	if !conf.Enabled || conf.XPUB == "" {
		return
	}
	if g, err := NewDepositAddressGenerator(conf); err == nil {
		depositAddrGen.Store(g)
	}
}
