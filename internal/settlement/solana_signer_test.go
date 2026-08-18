package settlement

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// randKey 生成一个随机 Ed25519 私钥（测试用），出错则失败。
func randKey(t *testing.T) solana.PrivateKey {
	t.Helper()
	priv, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("NewRandomPrivateKey: %v", err)
	}
	return priv
}

// newTestSolanaSigner 生成一把随机 Ed25519 私钥并构造软件签名器（blockhash 默认返回零哈希，
// 离线测试无需真实节点）。
func newTestSolanaSigner(t *testing.T) (*SolanaSigner, solana.PrivateKey) {
	t.Helper()
	priv := randKey(t)
	signer, err := NewSolanaSigner(priv.String())
	if err != nil {
		t.Fatalf("NewSolanaSigner: %v", err)
	}
	return signer, priv
}

// TestSolanaSignerNativeSOL 验证原生 SOL 转账：签名后可被反解、公钥验签通过、指令为 SystemProgram。
func TestSolanaSignerNativeSOL(t *testing.T) {
	signer, priv := newTestSolanaSigner(t)
	payer := priv.PublicKey()
	recipient := randKey(t).PublicKey()

	// 1 SOL = 1e9 lamports（9 decimals）。
	tx := &UnsignedTx{Chain: ChainSOL, To: recipient.String(), Asset: "SOL", Amount: AssetAmountFromInt64(1, 9)}
	raw, err := signer.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("Sign SOL: %v", err)
	}
	if raw == "" {
		t.Fatal("Sign 返回空交易")
	}
	// raw 应为 base58；反解为交易后应能验签。
	rawBytes, err := base58Decode(raw)
	if err != nil {
		t.Fatalf("base58 反解失败（Solana sendTransaction 收 base58）: %v", err)
	}
	parsed, err := solana.TransactionFromBytes(rawBytes)
	if err != nil {
		t.Fatalf("TransactionFromBytes: %v", err)
	}
	if len(parsed.Signatures) == 0 {
		t.Fatal("交易无签名")
	}
	msg, err := parsed.Message.MarshalBinary()
	if err != nil {
		t.Fatalf("Message.MarshalBinary: %v", err)
	}
	if !payer.Verify(msg, parsed.Signatures[0]) {
		t.Fatal("签名验签失败：签名者公钥与签名不匹配")
	}
	// 首条指令应为 SystemProgram 的 Transfer（program=SystemProgramID）。
	if len(parsed.Message.Instructions) == 0 {
		t.Fatal("交易无指令")
	}
	progIdx := parsed.Message.Instructions[0].ProgramIDIndex
	if got := parsed.Message.AccountKeys[progIdx]; !got.Equals(solana.SystemProgramID) {
		t.Fatalf("指令 program 应为 SystemProgram，实际 %s", got.String())
	}
	// 指令 data 为原始字节（非 base58）：前 4 字节为指令索引（2=Transfer），后 8 字节为 lamports（小端）。
	data := []byte(parsed.Message.Instructions[0].Data)
	if len(data) < 12 {
		t.Fatalf("指令 data 长度不足: %d", len(data))
	}
	if idx := uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24; idx != 2 {
		t.Fatalf("SystemProgram 指令索引应为 2(Transfer)，实际 %d", idx)
	}
	var lamports uint64
	for i := 0; i < 8; i++ {
		lamports |= uint64(data[4+i]) << (8 * i)
	}
	if lamports != 1_000_000_000 {
		t.Fatalf("转账 lamports 应为 1e9，实际 %d", lamports)
	}
}

// TestSolanaSignerSPLUSDC 验证 SPL/USDC 转账：含幂等 ATA 创建指令 + TokenProgram transfer，
// 目标 ATA 由 recipient 钱包派生。
func TestSolanaSignerSPLUSDC(t *testing.T) {
	signer, priv := newTestSolanaSigner(t)
	payer := priv.PublicKey()
	recipient := randKey(t).PublicKey()
	mintPub := solana.MustPublicKeyFromBase58(solanaUSDCContractMainnet)

	// 1 USDC = 1e6（6 decimals）。
	tx := &UnsignedTx{Chain: ChainSOL, To: recipient.String(), Asset: "USDC", Amount: AssetAmountFromInt64(1, 6)}
	raw, err := signer.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("Sign USDC: %v", err)
	}
	rawBytes, err := base58Decode(raw)
	if err != nil {
		t.Fatalf("base58 反解失败: %v", err)
	}
	parsed, err := solana.TransactionFromBytes(rawBytes)
	if err != nil {
		t.Fatalf("TransactionFromBytes: %v", err)
	}
	// 应含两条指令：① 幂等 ATA 创建（program=AssociatedTokenProgramID），② Token transfer（program=TokenProgramID）。
	if len(parsed.Message.Instructions) < 2 {
		t.Fatalf("SPL 转账应含 ATA 创建 + transfer 两条指令，实际 %d", len(parsed.Message.Instructions))
	}
	prog0 := parsed.Message.AccountKeys[parsed.Message.Instructions[0].ProgramIDIndex]
	if !prog0.Equals(solana.SPLAssociatedTokenAccountProgramID) {
		t.Fatalf("首条指令应为 AssociatedTokenProgram（ATA 创建），实际 %s", prog0.String())
	}
	prog1 := parsed.Message.AccountKeys[parsed.Message.Instructions[1].ProgramIDIndex]
	if !prog1.Equals(solana.TokenProgramID) {
		t.Fatalf("次条指令应为 TokenProgram（transfer），实际 %s", prog1.String())
	}
	// 验签。
	msg, err := parsed.Message.MarshalBinary()
	if err != nil {
		t.Fatalf("Message.MarshalBinary: %v", err)
	}
	if !payer.Verify(msg, parsed.Signatures[0]) {
		t.Fatal("SPL 交易签名验签失败")
	}
	// 目标 ATA 应等于 recipient 钱包的 USDC ATA。
	wantDst, _, err := solana.FindAssociatedTokenAddress(recipient, mintPub)
	if err != nil {
		t.Fatalf("FindAssociatedTokenAddress: %v", err)
	}
	if got := parsed.Message.AccountKeys[parsed.Message.Instructions[1].Accounts[1]]; !got.Equals(wantDst) {
		t.Fatalf("transfer 目标 ATA 应为 %s，实际 %s", wantDst.String(), got.String())
	}
}

// TestSolanaSignerF5Boundary 验证 F5 边界：零/负金额被拒。
func TestSolanaSignerF5Boundary(t *testing.T) {
	signer, _ := newTestSolanaSigner(t)
	recipient := randKey(t).PublicKey()
	for _, amt := range []AssetAmount{
		AssetAmount{},               // 零
		AssetAmountFromInt64(-1, 9), // 负（人类单位）
	} {
		tx := &UnsignedTx{Chain: ChainSOL, To: recipient.String(), Asset: "SOL", Amount: amt}
		if _, err := signer.Sign(context.Background(), tx); err == nil {
			t.Fatalf("零/负金额应被拒绝，实际通过: %+v", amt)
		}
	}
}

// TestSolanaSignerUnsupportedChain 验证非 SOL 链由调用方走其它签名器（返回错误）。
func TestSolanaSignerUnsupportedChain(t *testing.T) {
	signer, _ := newTestSolanaSigner(t)
	tx := &UnsignedTx{Chain: ChainETH, To: "0xabc", Asset: "ETH", Amount: AssetAmountFromInt64(1, 18)}
	if _, err := signer.Sign(context.Background(), tx); err == nil {
		t.Fatal("SolanaSigner 不应处理非 SOL 链")
	}
}

// TestSolanaKeyDecode 验证私钥 base58 与 hex 两种格式均可解析。
func TestSolanaKeyDecode(t *testing.T) {
	priv := randKey(t)
	if _, err := decodeSolanaKey(priv.String()); err != nil {
		t.Fatalf("base58 私钥解析失败: %v", err)
	}
	hexKey := hex.EncodeToString(priv[:])
	if _, err := decodeSolanaKey(hexKey); err != nil {
		t.Fatalf("hex 私钥解析失败: %v", err)
	}
	if _, err := decodeSolanaKey(""); err == nil {
		t.Fatal("空私钥应被拒绝")
	}
	if _, err := decodeSolanaKey("not-a-valid-key!!"); err == nil {
		t.Fatal("非法私钥应被拒绝")
	}
}

// TestSolanaHSMInterface 验证 HSM 接入点（SolanaKeySigner 接口 + 注册表）可按 keyID 注入后端。
func TestSolanaHSMInterface(t *testing.T) {
	// 自定义后端：签名器实现 SolanaKeySigner。
	priv := randKey(t)
	backend := &softwareSolanaSigner{priv: priv, pub: priv.PublicKey()}
	RegisterExternalSolanaSigner("hsm-key-1", backend)
	got, ok := lookupExternalSolanaSigner("hsm-key-1")
	if !ok {
		t.Fatal("注册表未找到注入的 Solana 签名后端")
	}
	if !got.Public().Equals(priv.PublicKey()) {
		t.Fatal("注入后端公钥不匹配")
	}
	// NewSolanaSignerWithBackend 走外部后端路径（生产 HSM 接入点）。
	s := NewSolanaSignerWithBackend(got)
	if s.backend != got {
		t.Fatal("NewSolanaSignerWithBackend 未使用注入后端")
	}
}

// TestToUint64 验证最小单位整数到 uint64 的安全转换（超出范围报错误，避免溢出 panic）。
func TestToUint64(t *testing.T) {
	if v, err := toUint64(AssetAmountFromInt64(1, 9)); err != nil || v != 1_000_000_000 {
		t.Fatalf("toUint64 错误: v=%d err=%v", v, err)
	}
	bigVal, _ := new(big.Int).SetString("99999999999999999999", 10)
	bigAmt := AssetAmount{Value: bigVal, Decimals: 0}
	if _, err := toUint64(bigAmt); err == nil {
		t.Fatal("超 uint64 金额应报错误")
	}
}

// 确保 base58 解码对 Solana 地址/交易（Bitcoin 字母表）与预期一致（回归：Solana 复用 BTC base58）。
func TestSolanaBase58Alphabet(t *testing.T) {
	// Solana 地址仅含 Bitcoin base58 字母表中的字符；用已知地址验证编解码往返。
	addr := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	dec, err := base58Decode(addr)
	if err != nil {
		t.Fatalf("base58Decode(Solana mint) 失败: %v", err)
	}
	if got := base58Encode(dec); got != addr {
		t.Fatalf("base58 往返不一致: %s != %s", got, addr)
	}
	if !strings.Contains(addr, "0") && !strings.Contains(addr, "O") && !strings.Contains(addr, "I") && !strings.Contains(addr, "l") {
		t.Skip("地址不含被排除字符，仅校验往返")
	}
}
