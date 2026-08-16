package settlement

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// KeySigner 抽象「在密钥不出域的安全模块内对 32 字节摘要做 secp256k1 ECDSA 签名」这一原语。
// 软件等价实现用本地私钥（私钥驻留进程内存安全域）；真实 HSM/KMS 实现把 SignDigest 换成设备
// / PKCS#11 / 云 KMS API 调用（如 AWS KMS Sign、PKCS#11 C_Sign、Hashicorp Vault transit
// sign），私钥永不离开安全模块。两条路径共用同一 realSigner 与 Signer 接口，生产仅需替换
// KeySigner 实现（见 NewExternalKeySigner / RegisterExternalSigner）。
type KeySigner interface {
	// SignDigest 对 32 字节摘要做 secp256k1 ECDSA 签名，返回 (r, s)。调用方负责低 S 规范化
	// 与 ETH recovery id 推导（见 recoverRecID）。真实设备通常返回 (r, s) 或 DER，DER 可经
	// ParseExternalDERSignature 解出 r/s。
	SignDigest(ctx context.Context, digest [32]byte) (r, s *big.Int, err error)
	// Public 返回该密钥对应的公钥（用于派生地址、确定 ETH recovery id、校验签名）。
	Public() *secp256k1.PublicKey
}

// softwareKeySigner 是 KeySigner 的软件等价实现（私钥驻留进程内存安全域，仅用于开发/演示；
// 生产替换为 HSM/KMS 后端，私钥不出域）。输出与 ecdsa.SignCompact 一致，仅丢弃 recovery id
// （统一由 realSigner 经公钥恢复推导，见 recoverRecID）。
type softwareKeySigner struct {
	priv *secp256k1.PrivateKey
}

func (k *softwareKeySigner) SignDigest(ctx context.Context, digest [32]byte) (*big.Int, *big.Int, error) {
	compact := ecdsa.SignCompact(k.priv, digest[:], false)
	r := new(big.Int).SetBytes(compact[1:33])
	s := new(big.Int).SetBytes(compact[33:65])
	return r, s, nil
}

func (k *softwareKeySigner) Public() *secp256k1.PublicKey { return k.priv.PubKey() }

// externalKeySigner 是 KeySigner 的真实 HSM/KMS 适配骨架：签名原语由调用方注入的 signFunc
// 完成（其内联调用真实设备 / PKCS#11 / 云 KMS），私钥永不离开安全模块；公钥由调用方提供
// （设备通常可导出公钥）。它是「离线签名边界」接入真实安全模块的唯一缝——生产无需改动
// settlement 其它代码，只需提供 signFunc（如包一层 AWS KMS Sign / C_Sign）。
type externalKeySigner struct {
	pub     *secp256k1.PublicKey
	signFunc func(ctx context.Context, digest [32]byte) (r, s *big.Int, err error)
}

func (k *externalKeySigner) SignDigest(ctx context.Context, digest [32]byte) (*big.Int, *big.Int, error) {
	return k.signFunc(ctx, digest)
}

func (k *externalKeySigner) Public() *secp256k1.PublicKey { return k.pub }

// NewExternalKeySigner 用提供的公钥与签名函数构造真实 HSM/KMS 后端适配。signFunc 内应调用
// 真实安全模块（返回 (r, s)；若设备返回 DER 编码签名，先经 ParseExternalDERSignature 解出
// r/s）。公钥用于派生地址与 ETH recovery id 推导，必须与该后端密钥对应。
//
// 示例（AWS KMS）：signFunc 调 kms.Sign(KeyId, Message=digest, MessageType=DIGEST) 得 DER，
// 再用 ParseExternalDERSignature 解出 (r,s)；公钥由 kms.GetPublicKey 取回并解析。
func NewExternalKeySigner(pub *secp256k1.PublicKey, signFunc func(ctx context.Context, digest [32]byte) (r, s *big.Int, err error)) KeySigner {
	return &externalKeySigner{pub: pub, signFunc: signFunc}
}

// ParseExternalDERSignature 把真实安全模块（如 AWS KMS、Vault、PKCS#11 返回 DER 的情形）返回的
// DER 编码 ECDSA 签名解出 (r, s)，供 externalKeySigner 的 signFunc 使用。
func ParseExternalDERSignature(der []byte) (r, s *big.Int, err error) {
	sig, err := ecdsa.ParseDERSignature(der)
	if err != nil {
		return nil, nil, fmt.Errorf("解析外部 DER 签名: %w", err)
	}
	rv := sig.R()
	sv := sig.S()
	rb := rv.Bytes()
	sb := sv.Bytes()
	return new(big.Int).SetBytes(rb[:]), new(big.Int).SetBytes(sb[:]), nil
}

// ---- 真实 HSM/KMS 后端注册表（配置驱动接入，无需改 settlement 代码） ----

var (
	externalSignerMu sync.Mutex
	externalSigners  = map[string]KeySigner{}
)

// RegisterExternalSigner 注册一个真实 HSM/KMS 后端（按 keyID 选择），供 HotWalletConfig
// SignerBackend="external" 时由 NewSigner 取用。生产部署期调用一次（如用 AWS KMS / PKCS#11
// 适配器构造 KeySigner 后注册），使离线签名边界在不修改 settlement 代码的前提下接入真实设备。
func RegisterExternalSigner(keyID string, backend KeySigner) {
	externalSignerMu.Lock()
	defer externalSignerMu.Unlock()
	externalSigners[keyID] = backend
}

// UnregisterExternalSigner 注销已注册的外部后端（主要用于测试清理与热替换）。
func UnregisterExternalSigner(keyID string) {
	externalSignerMu.Lock()
	defer externalSignerMu.Unlock()
	delete(externalSigners, keyID)
}

func lookupExternalSigner(keyID string) (KeySigner, bool) {
	externalSignerMu.Lock()
	defer externalSignerMu.Unlock()
	s, ok := externalSigners[keyID]
	return s, ok
}

// ---- 辅助 ----

// parseSignerKey 把 32 字节 hex 私钥（可选 0x 前缀）解析为 secp256k1 私钥。
func parseSignerKey(hexKey string) (*secp256k1.PrivateKey, error) {
	k := strings.TrimPrefix(hexKey, "0x")
	b, err := hex.DecodeString(k)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("invalid hot_wallet.signer_key: must be 32-byte hex private key: %w", err)
	}
	return secp256k1.PrivKeyFromBytes(b), nil
}

// digest32 把 32 字节摘要切片转为定长数组（SignDigest 入参）；入参须恰为 32 字节。
func digest32(b []byte) [32]byte {
	var d [32]byte
	copy(d[:], b)
	return d
}

// recoverRecID 由 (digest, r, s) 与已知公钥反推 secp256k1 recovery id（0 或 1）。真实 HSM/KMS
// 通常只返回 (r, s) 不含 recovery id，故统一用公钥匹配推导（软件后端亦复用，结果与 SignCompact
// 的 recID 一致，并经已知 EIP-155 向量校验）。低 S 规范化应在调用前完成。
func recoverRecID(digest [32]byte, r, s *big.Int, pub *secp256k1.PublicKey) (int, error) {
	rb := r.Bytes()
	if len(rb) < 32 {
		rb = append(make([]byte, 32-len(rb)), rb...)
	}
	sb := s.Bytes()
	if len(sb) < 32 {
		sb = append(make([]byte, 32-len(sb)), sb...)
	}
	for recID := 0; recID <= 1; recID++ {
		compact := make([]byte, 65)
		compact[0] = byte(27 + recID)
		copy(compact[1:33], rb)
		copy(compact[33:], sb)
		if p, _, err := ecdsa.RecoverCompact(compact, digest[:]); err == nil && p.IsEqual(pub) {
			return recID, nil
		}
	}
	return 0, fmt.Errorf("无法推导 ETH recovery id：签名与签名者公钥不匹配")
}
