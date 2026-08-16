package settlement

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// hsmSignServer 启动一个内存签名服务（复用生产 SigningService，按 knownVectorPriv 持有密钥），
// 按 der=true 返回 {signature: DER-hex} 否则返回 {r,s}(hex)。用于验证 remoteHSMKeySigner
// 两条解析路径与软件后端产出一致。
func hsmSignServer(t *testing.T, der bool) *httptest.Server {
	t.Helper()
	svc, err := NewSigningServiceFromKey(knownVectorPriv)
	if err != nil {
		t.Fatalf("NewSigningServiceFromKey: %v", err)
	}
	if der {
		if err := svc.SetResponseMode("der"); err != nil {
			t.Fatalf("SetResponseMode: %v", err)
		}
	}
	return httptest.NewServer(svc.Handler())
}

// softwareSign 用进程内软件后端对 digest 签名，作为 HSM 路径的对照基准（确定性 nonce，输出一致）。
func softwareSign(t *testing.T, digest [32]byte) (*big.Int, *big.Int) {
	t.Helper()
	priv, err := parseSignerKey(knownVectorPriv)
	if err != nil {
		t.Fatalf("parseSignerKey: %v", err)
	}
	r, s, err := (&softwareKeySigner{priv: priv}).SignDigest(context.Background(), digest)
	if err != nil {
		t.Fatalf("software sign: %v", err)
	}
	return r, s
}

// TestRemoteHSMKeySignerRSPath 验证 remote-http 后端解析 {r,s} 响应路径：与软件后端对同一
// digest 产出完全一致（确定性 ECDSA），且签名可恢复出配置公钥（recoverRecID 在 realSigner 中可用）。
func TestRemoteHSMKeySignerRSPath(t *testing.T) {
	priv, _ := parseSignerKey(knownVectorPriv)
	var digest [32]byte
	copy(digest[:], mustHex("aaaaaaaabbbbbbbbccccccccdddddddd11111111222222223333333344444444"))

	srv := hsmSignServer(t, false)
	defer srv.Close()

	backend, err := NewRemoteHSMKeySigner(priv.PubKey(), srv.URL, "")
	if err != nil {
		t.Fatalf("NewRemoteHSMKeySigner: %v", err)
	}
	r, s, err := backend.SignDigest(context.Background(), digest)
	if err != nil {
		t.Fatalf("SignDigest (rs): %v", err)
	}
	expR, expS := softwareSign(t, digest)
	if r.Cmp(expR) != 0 || s.Cmp(expS) != 0 {
		t.Fatalf("rs path mismatch:\n got (%x,%x)\nwant (%x,%x)", r, s, expR, expS)
	}
	// 复原公钥须等于配置公钥（保证真实签名可被 recoverRecID 推导 recovery id）。
	if !pubRecovers(digest, r, s, priv.PubKey()) {
		t.Fatalf("rs path: recovered pubkey != configured pubkey")
	}
}

// TestRemoteHSMKeySignerDERPath 验证 remote-http 后端解析 {signature: DER} 响应路径：与软件
// 后端产出完全一致（ParseExternalDERSignature 把 DER 解出 (r,s)）。
func TestRemoteHSMKeySignerDERPath(t *testing.T) {
	priv, _ := parseSignerKey(knownVectorPriv)
	var digest [32]byte
	copy(digest[:], mustHex("bbbbbbbbaaaaaaaa111111112222222233333333444444445555555566666666"))

	srv := hsmSignServer(t, true)
	defer srv.Close()

	backend, err := NewRemoteHSMKeySigner(priv.PubKey(), srv.URL, "")
	if err != nil {
		t.Fatalf("NewRemoteHSMKeySigner: %v", err)
	}
	r, s, err := backend.SignDigest(context.Background(), digest)
	if err != nil {
		t.Fatalf("SignDigest (der): %v", err)
	}
	expR, expS := softwareSign(t, digest)
	if r.Cmp(expR) != 0 || s.Cmp(expS) != 0 {
		t.Fatalf("der path mismatch:\n got (%x,%x)\nwant (%x,%x)", r, s, expR, expS)
	}
	if !pubRecovers(digest, r, s, priv.PubKey()) {
		t.Fatalf("der path: recovered pubkey != configured pubkey")
	}
}

// pubRecovers 用 (digest, r, s) 反推 secp256k1 公钥，校验是否与 want 相等（验证 recovery 可行性）。
func pubRecovers(digest [32]byte, r, s *big.Int, want *secp256k1.PublicKey) bool {
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
		if p, _, err := ecdsa.RecoverCompact(compact, digest[:]); err == nil && p.IsEqual(want) {
			return true
		}
	}
	return false
}

// TestRemoteHSMKeySignerAuth 验证 apiKey 作为 Bearer Token 注入 Authorization 头。
func TestRemoteHSMKeySignerAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"r": "01", "s": "02"})
	}))
	defer srv.Close()

	priv, _ := parseSignerKey(knownVectorPriv)
	backend, err := NewRemoteHSMKeySigner(priv.PubKey(), srv.URL, "secret-token")
	if err != nil {
		t.Fatalf("NewRemoteHSMKeySigner: %v", err)
	}
	var digest [32]byte
	if _, _, err := backend.SignDigest(context.Background(), digest); err != nil {
		t.Fatalf("SignDigest: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization 头应为 Bearer secret-token，实际 %q", gotAuth)
	}
}

// TestNewHSMKeySignerConfigDriven 验证按 HotWalletConfig.HSM 配置驱动构造后端：remote-http
// 成功；缺 kind / 未知 kind / 非法公钥 均报错（fail-degraded，网关回退节点侧签名广播）。
func TestNewHSMKeySignerConfigDriven(t *testing.T) {
	priv, _ := parseSignerKey(knownVectorPriv)
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())

	if _, err := newHSMKeySigner(HotWalletConfig{HSM: HSMConfig{Kind: "remote-http", Endpoint: "https://hsm/sign", PublicKey: pubHex}}); err != nil {
		t.Fatalf("remote-http 构造应成功: %v", err)
	}
	if _, err := newHSMKeySigner(HotWalletConfig{HSM: HSMConfig{Endpoint: "x", PublicKey: pubHex}}); err == nil {
		t.Fatalf("缺 kind 应报错")
	}
	if _, err := newHSMKeySigner(HotWalletConfig{HSM: HSMConfig{Kind: "aws-kms", Endpoint: "x", PublicKey: pubHex}}); err == nil {
		t.Fatalf("未知 kind 应报错")
	}
	if _, err := newHSMKeySigner(HotWalletConfig{HSM: HSMConfig{Kind: "remote-http", Endpoint: "x", PublicKey: "not-hex"}}); err == nil {
		t.Fatalf("非法公钥应报错")
	}
	if _, err := NewRemoteHSMKeySigner(priv.PubKey(), "", ""); err == nil {
		t.Fatalf("空 endpoint 应报错")
	}
	if _, err := NewRemoteHSMKeySigner(nil, "https://x", ""); err == nil {
		t.Fatalf("空公钥应报错")
	}
}

// TestNewRealSignerExternalAutoRegistersHSM 验证 SignerBackend="external" 且全局注册表未命中时，
// 按 HotWalletConfig.HSM 自动构造并注册真实后端（无需部署方手写 RegisterExternalSigner），
// 且其 ETH 签名产出与软件后端一致（确定性签名 → 与 knownVectorRaw 精确相同，可恢复出配置公钥）。
func TestNewRealSignerExternalAutoRegistersHSM(t *testing.T) {
	priv, _ := parseSignerKey(knownVectorPriv)
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	keyID := "hsm-auto-key"

	// 确保注册表干净。
	UnregisterExternalSigner(keyID)
	defer UnregisterExternalSigner(keyID)

	srv := hsmSignServer(t, false)
	defer srv.Close()

	conf := HotWalletConfig{
		SignerKey:     keyID,
		SignerBackend: "external",
		HSM:           HSMConfig{Kind: "remote-http", Endpoint: srv.URL, PublicKey: pubHex},
	}
	// 构造阶段按 HSMConfig 自动构造并注册真实后端（不发起网络调用，应成功）。
	s, err := newRealSignerWithSource(conf, SignerSources{})
	if err != nil {
		t.Fatalf("external+auto hsm 构造应成功: %v", err)
	}
	// 验证已注册进全局注册表。
	if _, ok := lookupExternalSigner(keyID); !ok {
		t.Fatalf("external+auto hsm 未写入全局注册表")
	}
	tx := &UnsignedTx{
		Chain: ChainETH, To: "0x3535353535353535353535353535353535353535",
		Amount: 1.0, Nonce: 9, GasPriceWei: 20000000000, GasLimit: 21000, ChainID: 1,
	}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("external+auto hsm 签名失败: %v", err)
	}
	// 确定性 ECDSA + 同一私钥 → 输出须与软件后端主向量精确相同。
	if raw != "0x"+knownVectorRaw {
		t.Fatalf("external+auto hsm raw 不符:\n got %s\nwant %s", raw, "0x"+knownVectorRaw)
	}
}
