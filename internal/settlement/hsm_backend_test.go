package settlement

import (
	"context"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// TestRealSignerExternalKeySignerMatchesSoftware 验证「真实 HSM/KMS 后端」接入缝与软件后端
// 产出完全一致：external 后端的 signFunc 内部用同一软件密钥模拟设备调用（生产替换为 AWS KMS
// Sign / PKCS#11 C_Sign），结果须与 knownVectorRaw 精确相同——证明替换 KeySigner 实现不改变
// 任何密码学输出，私钥永不离开后端闭包。
func TestRealSignerExternalKeySignerMatchesSoftware(t *testing.T) {
	priv, err := parseSignerKey(knownVectorPriv)
	if err != nil {
		t.Fatalf("parseSignerKey: %v", err)
	}
	keyID := "kms-key-1"
	// 真实设备适配骨架：仅 signFunc 内联调用安全模块。此处用软件密钥模拟，生产替换为真实调用。
	backend := NewExternalKeySigner(priv.PubKey(), func(ctx context.Context, digest [32]byte) (*big.Int, *big.Int, error) {
		compact := ecdsa.SignCompact(priv, digest[:], false)
		return new(big.Int).SetBytes(compact[1:33]), new(big.Int).SetBytes(compact[33:65]), nil
	})
	RegisterExternalSigner(keyID, backend)
	defer UnregisterExternalSigner(keyID)

	s, err := newRealSignerWithSource(HotWalletConfig{SignerKey: keyID, SignerBackend: "external"}, SignerSources{})
	if err != nil {
		t.Fatalf("newRealSignerWithSource external: %v", err)
	}
	tx := &UnsignedTx{
		Chain: ChainETH, To: "0x3535353535353535353535353535353535353535",
		Amount: 1.0, Nonce: 9, GasPriceWei: 20000000000, GasLimit: 21000, ChainID: 1,
	}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("sign ETH via external backend: %v", err)
	}
	if raw != "0x"+knownVectorRaw {
		t.Fatalf("external backend raw mismatch:\n got %s\nwant %s", raw, "0x"+knownVectorRaw)
	}
}

// TestRealSignerExternalBTC 验证 BTC 路径（UTXO 选择 + SIGHASH_ALL）在 external 后端下同样
// 工作：签名经外部 KeySigner 完成，复用独立解析 + ecdsa.Verify + 金额守恒校验。
func TestRealSignerExternalBTC(t *testing.T) {
	priv, err := parseSignerKey(btcTestPriv)
	if err != nil {
		t.Fatalf("parseSignerKey: %v", err)
	}
	keyID := "kms-btc-1"
	backend := NewExternalKeySigner(priv.PubKey(), func(ctx context.Context, d [32]byte) (*big.Int, *big.Int, error) {
		c := ecdsa.SignCompact(priv, d[:], false)
		return new(big.Int).SetBytes(c[1:33]), new(big.Int).SetBytes(c[33:65]), nil
	})
	RegisterExternalSigner(keyID, backend)
	defer UnregisterExternalSigner(keyID)

	s, err := newRealSignerWithSource(HotWalletConfig{SignerKey: keyID, SignerBackend: "external"}, SignerSources{})
	if err != nil {
		t.Fatalf("newRealSignerWithSource: %v", err)
	}
	pub := s.key.Public()
	utxos := btcTestUTXOs(t, pub)
	tx := &UnsignedTx{Chain: ChainBTC, To: deriveP2WPKHAddress(pub), Amount: 0.6, Asset: "BTC", UTXOs: utxos, FeeRatePerKB: 1000}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("sign BTC via external backend: %v", err)
	}
	verifyBTCSignatures(t, raw, utxos, pub.SerializeCompressed())
	verifyBTCValueConservation(t, raw, utxos, 0.6)
}

// TestRealSignerExternalUnregisteredFails 验证 SignerBackend="external" 但对应后端未注册时
// 返回错误（网关据此回退节点侧签名广播，fail-degraded）。
func TestRealSignerExternalUnregisteredFails(t *testing.T) {
	if _, err := newRealSignerWithSource(HotWalletConfig{SignerKey: "not-registered", SignerBackend: "external"}, SignerSources{}); err == nil {
		t.Fatalf("expected error for unregistered external signer")
	}
}

// TestParseExternalDERSignature 验证 ParseExternalDERSignature 能从 DER 解出 (r,s)，用于适配
// 真实安全模块（AWS KMS / Vault / PKCS#11 多返回 DER）的签名结果。
func TestParseExternalDERSignature(t *testing.T) {
	r := new(big.Int).SetBytes(mustHex("28ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276"))
	s := new(big.Int).SetBytes(mustHex("67cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"))
	der := derEncode(r, s)
	pr, ps, err := ParseExternalDERSignature(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pr.Cmp(r) != 0 || ps.Cmp(s) != 0 {
		t.Fatalf("DER round-trip mismatch: r=%x/%x s=%x/%x", pr, r, ps, s)
	}
}
