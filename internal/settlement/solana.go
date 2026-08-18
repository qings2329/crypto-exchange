package settlement

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
)

// deriveSolanaAddress 按 userID 确定性派生 Solana 风格充值地址（Ed25519 公钥 base58 编码）。
//
// 说明：Solana 采用 Ed25519 曲线，与现有 secp256k1 BIP32 xpub 派生体系不兼容，故无法复用
// 比特币/以太坊式的「xpub 非硬化子派生」。此处按 userID 确定性派生（仅持公钥，私钥不入任何
// 存储，与「进程不持私钥」HSM 模型一致）。Mock 网关场景下足够；生产接入真实 Solana 节点时，
// 应替换为 HSM 导出的 Ed25519 扩展公钥做非硬化子派生。
func deriveSolanaAddress(userID int64) string {
	seed := sha256.Sum256([]byte(fmt.Sprintf("solana-deposit:%d", userID)))
	priv := ed25519.NewKeyFromSeed(seed[:32])
	pub := priv.Public().(ed25519.PublicKey)
	// base58Encode 使用与 Solana 一致的 Bitcoin 字母表（见 btc_signer.go）。
	return base58Encode(pub)
}
