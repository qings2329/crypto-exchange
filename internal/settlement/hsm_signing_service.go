package settlement

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// SigningService 是「内部签名服务」的可部署实现——生产里它前置在真实 HSM/KMS 之前，对外仅
// 暴露「对 32 字节摘要做 ECDSA 签名」的 HTTPS 端点；私钥驻留本服务进程内存（真实部署替换为
// 调用 PKCS#11 / AWS KMS / Vault transit 的签名 API 即可，对外契约不变）。
//
// 它与 remoteHSMKeySigner 构成完整闭环：本服务 Hold 私钥并对 digest 签名，网关侧的
// remoteHSMKeySigner 把离线签名器的 SignDigest 转成一次对 /sign 的调用。签名数学是真实
// secp256k1 ECDSA（确定性 nonce，RFC6979），产出可被任意节点/钱包独立验证、可恢复出本服务
// 导出公钥的有效签名——即「部署真实 HSM 并验证签名」的本地可运行等价物。
type SigningService struct {
	priv *secp256k1.PrivateKey
	// mode 决定 /sign 响应形态："rs" → {r,s}(hex)（默认，remoteHSMKeySigner 直接解析）；
	// "der" → {signature: DER-hex}（AWS KMS Sign / Vault transit 常见返回，经
	// ParseExternalDERSignature 解出）。两种形态密码学等价、可互验。
	mode string
}

// NewSigningService 生成一枚全新 secp256k1 私钥（模拟 HSM 密钥生成/注入）。生产应改为从
// 真实安全模块导入或由其托管私钥（见 SetResponseMode 注释）。
func NewSigningService() *SigningService {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		// secp256k1 密钥生成仅在系统熵耗尽时失败，属不可恢复环境错误。
		panic(fmt.Errorf("生成 HSM 密钥失败: %w", err))
	}
	return &SigningService{priv: priv, mode: "rs"}
}

// NewSigningServiceFromKey 从 32 字节 hex 私钥（可选 0x 前缀）构造服务，便于复用已知密钥
// （如测试向量，或从真实 HSM 导出的密钥材料）。
func NewSigningServiceFromKey(hexKey string) (*SigningService, error) {
	priv, err := parseSignerKey(hexKey)
	if err != nil {
		return nil, fmt.Errorf("签名服务加载私钥失败: %w", err)
	}
	return &SigningService{priv: priv, mode: "rs"}, nil
}

// Public 返回服务持有的公钥（用于地址派生、ETH recovery id 推导、签名校验）。
func (s *SigningService) Public() *secp256k1.PublicKey { return s.priv.PubKey() }

// PublicKeyHex 返回压缩公钥 hex（运营把它填入 HSM_PUBLIC_KEY 环境变量，与网关侧配置对齐）。
func (s *SigningService) PublicKeyHex() string {
	return hex.EncodeToString(s.priv.PubKey().SerializeCompressed())
}

// SetResponseMode 设置 /sign 的响应形态："rs" 或 "der"。非法值返回错误。
func (s *SigningService) SetResponseMode(mode string) error {
	if mode != "rs" && mode != "der" {
		return fmt.Errorf("签名服务响应模式仅支持 rs/der，收到 %q", mode)
	}
	s.mode = mode
	return nil
}

// signDigest 对 32 字节摘要做真实 secp256k1 ECDSA 签名，返回 (r, s)（与软件后端同一原语，
// 仅私钥驻留本服务而非 settlement 进程——这正是离线签名边界要隔离的）。
func (s *SigningService) signDigest(digest [32]byte) (*big.Int, *big.Int, error) {
	return (&softwareKeySigner{priv: s.priv}).SignDigest(nil, digest)
}

// Handler 返回内部签名服务的 HTTP 处理器，提供：
//   - POST /sign：请求体 {"digest":"<64 hex>"}，按 mode 返回 {"r","s"} 或 {"signature":"<DER hex>"}。
//   - GET  /pubkey：返回 {"public_key":"<压缩 hex>"}，供运营核对/注入。
//   - GET  /health：就绪探针。
func (s *SigningService) Handler() http.Handler {
	mux := http.NewServeMux()
	// /sign 是约定的签名端点（HSM_ENDPOINT 指向它）；同时接受根路径 POST，便于单测与
	// 把服务直接挂在根下的简易部署。
	mux.HandleFunc("/sign", s.handleSign)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "" {
			s.handleSign(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/pubkey", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"public_key": s.PublicKeyHex()})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func (s *SigningService) handleSign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Digest string `json:"digest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	d, err := hex.DecodeString(in.Digest)
	if err != nil || len(d) != 32 {
		http.Error(w, "digest 须为 32 字节 hex", http.StatusBadRequest)
		return
	}
	var digest [32]byte
	copy(digest[:], d)

	rr, ss, err := s.signDigest(digest)
	if err != nil {
		http.Error(w, "sign failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.mode == "der" {
		var modR, modS secp256k1.ModNScalar
		modR.SetByteSlice(rr.Bytes())
		modS.SetByteSlice(ss.Bytes())
		der := ecdsa.NewSignature(&modR, &modS).Serialize()
		_ = json.NewEncoder(w).Encode(map[string]string{"signature": hex.EncodeToString(der)})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{
		"r": hex.EncodeToString(rr.Bytes()),
		"s": hex.EncodeToString(ss.Bytes()),
	})
}

// writeJSON 是签名服务内部的小工具：把 v 以 JSON 写入 w 并设置 200。
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
