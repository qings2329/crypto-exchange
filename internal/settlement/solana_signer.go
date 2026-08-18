package settlement

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
)

// SolanaKeySigner 是 Solana 离线签名边界的「签名原语后端」：软件实现用进程内 Ed25519 私钥
// 对交易做本地签名（私钥不出域）；生产可另实现「对交易/摘要做远程签名」的 HSM/KMS 后端，
// 经 RegisterExternalSolanaSigner 注入（与 secp256k1 的 RegisterExternalSigner 对称）——这正是
// 「HSM Ed25519（留接口）」的接入点：替换 SignTransaction 的实现即可，其余 settlement 代码不变。
type SolanaKeySigner interface {
	// SignTransaction 在签名域内对交易做离线签名（写入 tx.Signatures），私钥不出域。
	SignTransaction(tx *solana.Transaction) error
	// Public 返回签名者公钥（热钱包地址），用于构造交易与校验。
	Public() solana.PublicKey
}

// softwareSolanaSigner 是 SolanaKeySigner 的软件实现：用内存 Ed25519 私钥对交易签名，
// 仅用于开发/演示与离线单测；生产替换为 HSM/KMS 后端。
type softwareSolanaSigner struct {
	priv solana.PrivateKey
	pub  solana.PublicKey
}

func (s *softwareSolanaSigner) SignTransaction(tx *solana.Transaction) error {
	_, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(s.pub) {
			p := s.priv
			return &p
		}
		return nil
	})
	return err
}

func (s *softwareSolanaSigner) Public() solana.PublicKey { return s.pub }

// —— 全局注册表（镜像 secp256k1 的 RegisterExternalSigner / lookupExternalSigner）——
// 生产 HSM/KMS 后端注册后，SolanaSigner 可经 keyID 取用，私钥永不离开安全模块。
var solanaSignerRegistry sync.Map // keyID string -> SolanaKeySigner

// RegisterExternalSolanaSigner 注入外部 Solana 签名后端（HSM/KMS）；keyID 用于查找。
func RegisterExternalSolanaSigner(keyID string, backend SolanaKeySigner) {
	solanaSignerRegistry.Store(keyID, backend)
}

// lookupExternalSolanaSigner 按 keyID 取注册表后端；未命中返回 (nil, false)。
func lookupExternalSolanaSigner(keyID string) (SolanaKeySigner, bool) {
	if v, ok := solanaSignerRegistry.Load(keyID); ok {
		return v.(SolanaKeySigner), true
	}
	return nil, false
}

// SolanaSigner 实现既有 Signer 接口（离线签名边界统一入口），用 Ed25519 对 Solana 交易做真实
// 签名，返回 base58 编码的已签交易（Solana sendTransaction 收 base58）。仅处理 ChainSOL；
// 其它链返回错误（由调用方走对应签名器）。
//
// 金额全程最小单位整数（#6）：原生 SOL 以 lamports（9 decimals）计，SPL 以 mint 最小单位（USDC 6
// decimals）计，统一经 toDecimals 缩放到标准小数位，无 float 中间量。
type SolanaSigner struct {
	backend   SolanaKeySigner
	blockhash func(ctx context.Context) (solana.Hash, error) // 最近区块哈希来源（真实节点；离线测试可注入）
}

// NewSolanaSigner 从 base58 或 hex 私钥构造软件签名器（开发/演示）。密钥非法返回错误，由
// 网关 fail-degraded 回退（不阻断其它链）。
func NewSolanaSigner(key string) (*SolanaSigner, error) {
	priv, err := decodeSolanaKey(key)
	if err != nil {
		return nil, err
	}
	backend := &softwareSolanaSigner{priv: priv, pub: priv.PublicKey()}
	return &SolanaSigner{
		backend:   backend,
		blockhash: func(ctx context.Context) (solana.Hash, error) { return solana.Hash{}, nil },
	}, nil
}

// NewSolanaSignerWithBackend 用外部签名后端（HSM/KMS）构造签名器；生产路径。
func NewSolanaSignerWithBackend(backend SolanaKeySigner) *SolanaSigner {
	return &SolanaSigner{
		backend:   backend,
		blockhash: func(ctx context.Context) (solana.Hash, error) { return solana.Hash{}, nil },
	}
}

// SetBlockhashSource 注入最近区块哈希来源（真实节点时由 JSONRPCClient 提供）；网关装配时调用。
func (s *SolanaSigner) SetBlockhashSource(fn func(ctx context.Context) (solana.Hash, error)) {
	if fn != nil {
		s.blockhash = fn
	}
}

// Sign 构造并签名一笔 Solana 提现交易（原生 SOL 或 SPL 代币），返回 base58 编码的已签交易。
func (s *SolanaSigner) Sign(ctx context.Context, tx *UnsignedTx) (string, error) {
	if tx.Chain != ChainSOL {
		return "", fmt.Errorf("SolanaSigner 不支持链 %s", tx.Chain)
	}
	if tx.Amount.Sign() <= 0 { // F5 边界：拒绝零/负金额
		return "", fmt.Errorf("Solana 提现金额必须为正")
	}
	mint, isNative := solanaMintByAsset(tx.Asset)
	if !isNative && tx.ContractAddress != "" {
		mint = tx.ContractAddress // 覆盖 SPL mint（UnsignedTx.ContractAddress 复用为 SOL 链 mint）
	}
	if !isNative && mint == "" {
		return "", fmt.Errorf("未知 SPL 资产 %q（请在 ContractAddress 指定 mint）", tx.Asset)
	}
	bh, err := s.blockhash(ctx)
	if err != nil {
		return "", fmt.Errorf("获取 Solana 最近区块哈希失败: %w", err)
	}
	payer := s.backend.Public()
	to, err := solana.PublicKeyFromBase58(tx.To)
	if err != nil {
		return "", fmt.Errorf("Solana 收款地址非法: %w", err)
	}

	var instructions []solana.Instruction
	if isNative {
		lamports, err := toUint64(tx.Amount.toDecimals(9))
		if err != nil {
			return "", fmt.Errorf("SOL 金额超出 uint64: %w", err)
		}
		ix, err := system.NewTransferInstruction(lamports, payer, to).ValidateAndBuild()
		if err != nil {
			return "", fmt.Errorf("构造 SOL 转账指令失败: %w", err)
		}
		instructions = append(instructions, ix)
	} else {
		mintPub, err := solana.PublicKeyFromBase58(mint)
		if err != nil {
			return "", fmt.Errorf("SPL mint 非法: %w", err)
		}
		srcATA, _, err := solana.FindAssociatedTokenAddress(payer, mintPub)
		if err != nil {
			return "", fmt.Errorf("计算源 ATA 失败: %w", err)
		}
		dstATA, _, err := solana.FindAssociatedTokenAddress(to, mintPub)
		if err != nil {
			return "", fmt.Errorf("计算目标 ATA 失败: %w", err)
		}
		// 目标 ATA 可能尚未创建：追加幂等创建指令（ATA 已存在时链上 no-op，避免首笔转账失败）。
		createIx, err := associatedtokenaccount.NewCreateIdempotentInstruction(payer, to, mintPub).ValidateAndBuild()
		if err != nil {
			return "", fmt.Errorf("构造 ATA 创建指令失败: %w", err)
		}
		instructions = append(instructions, createIx)
		amount, err := toUint64(tx.Amount.toDecimals(6))
		if err != nil {
			return "", fmt.Errorf("SPL 金额超出 uint64: %w", err)
		}
		transferIx, err := token.NewTransferInstruction(amount, srcATA, dstATA, payer, nil).ValidateAndBuild()
		if err != nil {
			return "", fmt.Errorf("构造 SPL 转账指令失败: %w", err)
		}
		instructions = append(instructions, transferIx)
	}

	txn, err := solana.NewTransaction(instructions, bh, solana.TransactionPayer(payer))
	if err != nil {
		return "", fmt.Errorf("构造 Solana 交易失败: %w", err)
	}
	if err := s.backend.SignTransaction(txn); err != nil {
		return "", fmt.Errorf("Solana 签名失败: %w", err)
	}
	raw, err := txn.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("Solana 交易序列化失败: %w", err)
	}
	// Solana 的 sendTransaction 收 base58（或 base64）编码交易；此处用与 Solana 一致的
	// Bitcoin base58 字母表（见 btc_signer.go 的 base58Encode）编码。
	return base58Encode(raw), nil
}

// decodeSolanaKey 解析 Ed25519 私钥：优先按 base58（Solana 钱包导出格式），否则按 hex（64 字符=32 字节）。
func decodeSolanaKey(key string) (solana.PrivateKey, error) {
	if key == "" {
		return nil, fmt.Errorf("Solana 私钥为空")
	}
	if pk, err := solana.PrivateKeyFromBase58(key); err == nil && len(pk) == 64 {
		return pk, nil
	}
	if b, err := hex.DecodeString(key); err == nil && (len(b) == 64 || len(b) == 32) {
		return solana.PrivateKey(b), nil
	}
	// 再尝试 base58（即便上面校验失败，给出明确错误）
	if _, err := solana.PrivateKeyFromBase58(key); err != nil {
		return nil, fmt.Errorf("Solana 私钥解析失败（需 base58 或 hex）: %w", err)
	}
	return nil, fmt.Errorf("Solana 私钥长度非法（需 32 字节）")
}

// toUint64 把最小单位整数金额安全转为 uint64；超出范围返回错误（避免溢出 panic）。
func toUint64(a AssetAmount) (uint64, error) {
	v := a.Value
	if v == nil {
		return 0, nil
	}
	if !v.IsUint64() {
		return 0, fmt.Errorf("金额 %s 超出 uint64 范围", v.String())
	}
	return v.Uint64(), nil
}

// 确保 SolanaSigner 实现 Signer 接口（编译期检查）。
var _ Signer = (*SolanaSigner)(nil)
