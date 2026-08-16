package settlement

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/coldlar/crypto-exchange/internal/pkg/keccak"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// secp256k1 曲线的阶 N 与其一半（用于 ECDSA 低 S 规范化，满足以太坊对可锻性的要求）。
var (
	secp256k1Order = new(big.Int).SetBytes(mustHex("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141"))
	secp256k1HalfN = new(big.Int).Rsh(secp256k1Order, 1)
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// realSigner 是离线签名边界的真实实现（软件 HSM 等价）：在本地用 secp256k1 对 ETH 交易做
// 真实 ECDSA 签名，返回可直接 SendRaw 广播的原始交易 hex。私钥仅驻留本进程内存（安全域），
// 不出域；生产环境应把 Sign 内部改成调用 HSM/KMS 的签名 API（同样的 Signer 接口），私钥
// 永不离开安全模块——这正是「离线签名边界」要隔离的东西。
//
// 当前支持：
//   - ETH（legacy / EIP-155）真实 RLP + Keccak-256 + ECDSA 签名；
//   - BTC（P2PKH legacy + P2WPKH segwit）UTXO 选择 + 找零 + SIGHASH_ALL 真实签名；
//   - TRON（TransferContract 原生 TRX 转账 / TriggerSmartContract TRC20 合约调用）protobuf raw_data
//     序列化 + SHA256 摘要 + secp256k1 可恢复 65 字节签名 + broadcasttransaction JSON。
//
// TRON 签名需要 TRONState 源（参考区块/时间戳）；未注入或取块失败时 Sign 返回错误，由网关
// 回退节点侧签名广播（fail-degraded）。
type realSigner struct {
	key            KeySigner // 实际签名原语后端：软件（进程内私钥）或真实 HSM/KMS（私钥不出域）。
	address        string    // 由公钥派生的 ETH 地址（0x…），仅用于可观测/日志与 Nonce 查询。
	ethChainID     uint64
	ethGasPriceWei uint64
	ethGasLimit    uint64
	utxoSource     UTXOSource     // BTC UTXO 来源（可选；为空则用 UnsignedTx.UTXOs 内联提供）。
	ethState       ETHStateSource // ETH Nonce/Gas 来源（可选；为空则按 UnsignedTx/配置兜底）。
	tronState      TRONStateSource // TRON 参考区块/时间戳来源（可选；为空则 TRON 签名失败→回退节点侧）。
	// 本地 pending nonce 计数器：首次签名向节点取当前计数，之后本地递增，避免并发/未确认
	// 期间重放碰撞。mu 保护；haveNonce=false 表示尚未从节点播种。
	mu        sync.Mutex
	nextNonce uint64
	haveNonce bool
}

// newRealSigner 从配置解析私钥并构造真实签名器；私钥非法/长度不对返回错误（→ fail-degraded）。
func newRealSigner(conf HotWalletConfig) (*realSigner, error) {
	return newRealSignerWithSource(conf, SignerSources{})
}

// newRealSignerWithSource 在 newRealSigner 基础上注入可选外部状态源（BTC UTXO 源 / ETH Nonce·Gas 源），
// 并按 SignerBackend 选择签名后端：
//   - "external"（或 "hsm-remote"/"kms"）：从注册表取部署方注入的真实 HSM/KMS 后端（私钥不出域）；
//     未注册则报错（fail-degraded，网关回退节点侧签名广播）。
//   - "software"（默认或空）：用 SignerKey（32 字节 hex）构造进程内软件签名后端（开发/演示）。
func newRealSignerWithSource(conf HotWalletConfig, sources SignerSources) (*realSigner, error) {
	var key KeySigner
	switch conf.SignerBackend {
	case "external", "hsm-remote", "kms":
		backend, ok := lookupExternalSigner(conf.SignerKey)
		if !ok {
			return nil, fmt.Errorf("external 签名后端 %q 未注册：请用 settlement.RegisterExternalSigner 注入真实 HSM/KMS 后端", conf.SignerKey)
		}
		key = backend
	default: // "software" 或空
		priv, err := parseSignerKey(conf.SignerKey)
		if err != nil {
			return nil, err
		}
		key = &softwareKeySigner{priv: priv}
	}
	return &realSigner{
		key:            key,
		address:        deriveETHAddress(key.Public()),
		ethChainID:     conf.EthChainID,
		ethGasPriceWei: conf.EthGasPriceWei,
		ethGasLimit:    conf.EthGasLimit,
		utxoSource:     sources.UTXOSource,
		ethState:       sources.ETHState,
		tronState:      sources.TRONState,
	}, nil
}

// Sign 在签名器域内对交易做真实签名，返回已签名的 raw transaction hex。
func (s *realSigner) Sign(ctx context.Context, tx *UnsignedTx) (string, error) {
	switch tx.Chain {
	case ChainETH:
		return s.signETH(ctx, tx)
	case ChainBTC:
		return s.signBTC(ctx, tx)
	case ChainTRON:
		return s.signTRON(ctx, tx)
	default:
		return "", fmt.Errorf("realSigner 不支持链 %s", tx.Chain)
	}
}

// signETH 构造一笔 ETH 交易，做 Keccak-256 摘要 + secp256k1 ECDSA 签名 + EIP-155（或 legacy）
// v 值，最后 RLP 编码为可直接 eth_sendRawTransaction 的 raw hex。
//
// Nonce / Gas 管理（真实链上状态）：Nonce 优先用 UnsignedTx.Nonce，否则向节点查
// eth_getTransactionCount("pending") 并本地递增（resolveNonce）；GasPrice 优先用 UnsignedTx，
// 否则向节点查 eth_gasPrice，再回退配置 eth_gas_price_wei；节点不可达均 fail-degraded 兜底。
func (s *realSigner) signETH(ctx context.Context, tx *UnsignedTx) (string, error) {
	chainID := tx.ChainID
	if chainID == 0 {
		chainID = s.ethChainID
	}
	gasPrice := tx.GasPriceWei
	if gasPrice == 0 && s.ethState != nil {
		if gp, err := s.ethState.GasPrice(ctx, ChainETH); err == nil {
			gasPrice = gp
		}
		// 节点不可达：忽略错误，继续用配置默认（fail-degraded）。
	}
	if gasPrice == 0 {
		gasPrice = s.ethGasPriceWei
	}
	if gasPrice == 0 {
		return "", fmt.Errorf("ETH 签名需要 gasPrice（UnsignedTx.GasPriceWei / 节点 eth_gasPrice / hot_wallet.eth_gas_price_wei 必须 > 0）")
	}
	gasLimit := tx.GasLimit
	if gasLimit == 0 {
		gasLimit = s.ethGasLimit
	}
	if gasLimit == 0 {
		gasLimit = 21000 // 简单转账默认 gas limit
	}
	nonce, err := s.resolveNonce(ctx, tx)
	if err != nil {
		return "", err
	}

	toBytes, err := parseETHAddress(tx.To)
	if err != nil {
		return "", err
	}
	valueWei := amountToWei(tx.Amount)

	// 待签摘要：对未签字段做 RLP 编码后取 Keccak-256。EIP-155 下列表含 chainID 与两个 0 占位，
	// legacy（chainID==0）下不含这三项。
	unsignedFields := [][]byte{
		rlpBigInt(big.NewInt(int64(nonce))),
		rlpBigInt(new(big.Int).SetUint64(gasPrice)),
		rlpBigInt(new(big.Int).SetUint64(gasLimit)),
		rlpAddress(toBytes),
		rlpBigInt(valueWei),
		rlpBytes(tx.Data),
	}
	if chainID != 0 {
		unsignedFields = append(unsignedFields,
			rlpBigInt(new(big.Int).SetUint64(chainID)),
			rlpBigInt(big.NewInt(0)),
			rlpBigInt(big.NewInt(0)),
		)
	}
	digest := keccak.Sum256(rlpEncodeList(unsignedFields))

	// secp256k1 ECDSA 签名（经 KeySigner：软件或真实 HSM/KMS 后端，私钥不出域）。
	r, sBig, err := s.key.SignDigest(ctx, digest)
	if err != nil {
		return "", fmt.Errorf("ETH 签名失败: %w", err)
	}
	// 低 S 规范化（以太坊要求 S 落在曲线阶的一半以内，抗交易可锻性）：S 过大则取补数。
	// recovery id 由公钥匹配推导（见 recoverRecID），无需手动翻转。
	if sBig.Cmp(secp256k1HalfN) > 0 {
		sBig = new(big.Int).Sub(secp256k1Order, sBig)
	}
	recID, err := recoverRecID(digest, r, sBig, s.key.Public())
	if err != nil {
		return "", err
	}
	sBytes := sBig.Bytes()
	if len(sBytes) < 32 {
		sBytes = append(make([]byte, 32-len(sBytes)), sBytes...)
	}

	// v 值：EIP-155 为 chainID*2+35+recID；legacy 为 27+recID。
	var v *big.Int
	if chainID != 0 {
		v = new(big.Int).Add(new(big.Int).SetUint64(chainID*2+35), big.NewInt(int64(recID)))
	} else {
		v = big.NewInt(int64(27 + recID))
	}

	// 已签交易 RLP：[nonce, gasPrice, gasLimit, to, value, data, v, r, s]，可 eth_sendRawTransaction 广播。
	raw := rlpEncodeList([][]byte{
		rlpBigInt(big.NewInt(int64(nonce))),
		rlpBigInt(new(big.Int).SetUint64(gasPrice)),
		rlpBigInt(new(big.Int).SetUint64(gasLimit)),
		rlpAddress(toBytes),
		rlpBigInt(valueWei),
		rlpBytes(tx.Data),
		rlpBigInt(v),
		rlpBigInt(r),
		rlpBytes(sBytes),
	})
	return "0x" + hex.EncodeToString(raw), nil
}

// resolveNonce 解析用于签名的 Nonce（ETH 真实 Nonce 管理）：
//   - UnsignedTx.Nonce 显式提供（>0）时直接采用（调用方自管 Nonce）。
//   - 否则：已本地播种则递增返回；未播种则向节点查 eth_getTransactionCount("pending") 作为
//     起点并缓存 nextNonce，之后本地递增（避免并发/未确认期间的重放碰撞）；节点不可达时
//     回退默认 0（fail-degraded，真实环境应与节点确认数保持一致）。
func (s *realSigner) resolveNonce(ctx context.Context, tx *UnsignedTx) (uint64, error) {
	if tx.Nonce != 0 {
		return tx.Nonce, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.haveNonce {
		n := s.nextNonce
		s.nextNonce++
		return n, nil
	}
	if s.ethState != nil {
		if n, err := s.ethState.Nonce(ctx, ChainETH, s.address); err == nil {
			s.nextNonce = n + 1
			s.haveNonce = true
			return n, nil
		}
		// 节点不可达：忽略错误，回退默认 0（fail-degraded）。
	}
	return 0, nil
}

// amountToWei 把以 ETH 为单位的金额转换为 wei（big.Int），避免 float64 精度损失。
func amountToWei(amount float64) *big.Int {
	f := new(big.Float).SetFloat64(amount)
	f.Mul(f, new(big.Float).SetInt64(1e18))
	wei, _ := f.Int(nil)
	if wei == nil {
		wei = big.NewInt(0)
	}
	return wei
}

// parseETHAddress 解析 20 字节 ETH 地址（剥离 0x 前缀）；空字符串表示合约创建。
func parseETHAddress(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return nil, nil // 合约创建
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return nil, fmt.Errorf("invalid ETH address %q", s)
	}
	return b, nil
}

// deriveETHAddress 由未压缩公钥派生 ETH 地址：keccak256(pubkey[1:]) 取后 20 字节。
func deriveETHAddress(pub *secp256k1.PublicKey) string {
	u := pub.SerializeUncompressed() // 65 字节：0x04 + x(32) + y(32)
	h := keccak.Sum256(u[1:])        // 去掉 0x04 前缀
	return "0x" + hex.EncodeToString(h[12:])
}

// ---------- RLP 编码（最小化，仅覆盖 ETH 交易所需的整数/字节串/列表） ----------

// rlpBytes 编码一个字节串：单字节 < 0x80 直接输出；否则加长度前缀。
func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	if len(b) <= 55 {
		return append([]byte{byte(0x80 + len(b))}, b...)
	}
	return append(append([]byte{byte(0xb7 + len(lenPrefix(len(b))))}, lenPrefix(len(b))...), b...)
}

// rlpBigInt 编码一个大整数（最小大端表示，0 编码为空串）。
func rlpBigInt(x *big.Int) []byte {
	if x == nil || x.Sign() == 0 {
		return []byte{0x80}
	}
	return rlpBytes(x.Bytes())
}

// rlpAddress 编码地址字段：20 字节地址或空（合约创建）。
func rlpAddress(addr []byte) []byte {
	if len(addr) == 0 {
		return []byte{0x80}
	}
	return rlpBytes(addr)
}

// rlpEncodeList 编码 RLP 列表：拼接各元素后加列表长度前缀。
func rlpEncodeList(items [][]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	if len(payload) <= 55 {
		return append([]byte{byte(0xc0 + len(payload))}, payload...)
	}
	return append(append([]byte{byte(0xf7 + len(lenPrefix(len(payload))))}, lenPrefix(len(payload))...), payload...)
}

// lenPrefix 把长度编码为大端字节串（不带前导零）。
func lenPrefix(n int) []byte {
	if n == 0 {
		return []byte{0x00}
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte(n & 0xff)}, b...)
		n >>= 8
	}
	return b
}
