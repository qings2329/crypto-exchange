package settlement

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/keccak"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// 已知 EIP-155 测试向量：私钥 32 字节全 0x46、nonce=9、gasPrice=20gwei、gasLimit=21000、
// to=0x3535…35、value=1 ETH、data=空、chainID=1。期望的 raw tx 来自以太坊签名规范。
const knownVectorPriv = "4646464646464646464646464646464646464646464646464646464646464646"
const knownVectorRaw = "f86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"

func TestRealSignerKnownEIP155Vector(t *testing.T) {
	s, err := newRealSigner(HotWalletConfig{SignerKey: knownVectorPriv})
	if err != nil {
		t.Fatalf("newRealSigner: %v", err)
	}
	tx := &UnsignedTx{
		Chain:       ChainETH,
		To:          "0x3535353535353535353535353535353535353535",
		Amount:      amt(ChainETH, 1.0),
		Asset:       "ETH",
		Nonce:       9,
		GasPriceWei: 20000000000, // 20 gwei
		GasLimit:    21000,
		ChainID:     1,
	}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if raw != "0x"+knownVectorRaw {
		t.Fatalf("EIP-155 raw mismatch:\n got %s\nwant %s", raw, "0x"+knownVectorRaw)
	}
}

// TestRealSignerRecoversToAddress 验证签名可恢复出签名者地址（真实签名 + 私钥不出域自洽）。
func TestRealSignerRecoversToAddress(t *testing.T) {
	s, err := newRealSigner(HotWalletConfig{SignerKey: knownVectorPriv})
	if err != nil {
		t.Fatalf("newRealSigner: %v", err)
	}
	tx := &UnsignedTx{
		Chain: ChainETH, To: "0x3535353535353535353535353535353535353535",
		Amount: amt(ChainETH, 1.0), Nonce: 9, GasPriceWei: 20000000000, GasLimit: 21000, ChainID: 1,
	}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	recovered, err := recoverETHAddressFromRaw(raw, tx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != s.address {
		t.Fatalf("recovered address %s != signer address %s", recovered, s.address)
	}
}

// TestNewSignerHSM 验证 hsm/kms 类型在私钥有效时返回真实签名器，私钥无效/缺失时返回 nil
// （网关回退节点侧签名广播，fail-degraded）。
func TestNewSignerHSM(t *testing.T) {
	valid := NewSigner(HotWalletConfig{Enabled: true, SignerType: "hsm", SignerKey: knownVectorPriv})
	if valid == nil {
		t.Fatalf("expected real signer for valid hsm key")
	}
	if _, ok := valid.(*realSigner); !ok {
		t.Fatalf("expected *realSigner, got %T", valid)
	}
	// 私钥缺失 → nil（fail-degraded）。
	if NewSigner(HotWalletConfig{Enabled: true, SignerType: "hsm"}) != nil {
		t.Fatalf("expected nil signer when key missing")
	}
	// 私钥非法 → nil（fail-degraded）。
	if NewSigner(HotWalletConfig{Enabled: true, SignerType: "hsm", SignerKey: "zzzz"}) != nil {
		t.Fatalf("expected nil signer for invalid key")
	}
}

// TestRealSignerUnsupportedChain 验证 TRON 暂未实现，签名返回错误 → 网关回退节点侧广播。
// （ETH/BTC 均已支持真实签名。）
func TestRealSignerUnsupportedChain(t *testing.T) {
	s, _ := newRealSigner(HotWalletConfig{SignerKey: knownVectorPriv})
	if _, err := s.Sign(context.Background(), &UnsignedTx{Chain: ChainTRON, To: "x", Amount: amt(ChainTRON, 1)}); err == nil {
		t.Fatalf("chain TRON should not be signable yet")
	}
}

// TestRealSignerRequiresGasPrice 验证缺 gasPrice（tx 与配置均无）时签名失败 → 回退广播。
func TestRealSignerRequiresGasPrice(t *testing.T) {
	s, _ := newRealSigner(HotWalletConfig{SignerKey: knownVectorPriv}) // 未配 eth_gas_price_wei
	if _, err := s.Sign(context.Background(), &UnsignedTx{Chain: ChainETH, To: "0x3535", Amount: amt(ChainETH, 1)}); err == nil {
		t.Fatalf("expected error when gasPrice missing")
	}
}

// fakeETHState 是 ETHStateSource 的内存假实现（用于真实 Nonce/Gas 管理单测）。
type fakeETHState struct {
	nonce      uint64
	gasPrice   uint64
	nonceCalls int
	gasCalls   int
}

func (f *fakeETHState) Nonce(ctx context.Context, chain Chain, account string) (uint64, error) {
	f.nonceCalls++
	return f.nonce, nil
}
func (f *fakeETHState) GasPrice(ctx context.Context, chain Chain) (uint64, error) {
	f.gasCalls++
	return f.gasPrice, nil
}

// TestRealSignerETHResolvesNonceGasFromNode 验证未显式提供 Nonce/GasPrice 时，签名器向节点
// 查 eth_getTransactionCount / eth_gasPrice，并把查询结果嵌入 raw（复用 RLP 反解 + 地址恢复校验）。
func TestRealSignerETHResolvesNonceGasFromNode(t *testing.T) {
	st := &fakeETHState{nonce: 12, gasPrice: 1_000_000_000} // 1 gwei
	s, err := newRealSignerWithSource(HotWalletConfig{SignerKey: knownVectorPriv}, SignerSources{ETHState: st})
	if err != nil {
		t.Fatalf("newRealSignerWithSource: %v", err)
	}
	tx := &UnsignedTx{Chain: ChainETH, To: "0x3535353535353535353535353535353535353535", Amount: amt(ChainETH, 1.0), Asset: "ETH"}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if st.nonceCalls != 1 || st.gasCalls != 1 {
		t.Fatalf("expected node nonce+gas queries (1 each), got nonceCalls=%d gasCalls=%d", st.nonceCalls, st.gasCalls)
	}
	n, err := extractETHNonce(raw)
	if err != nil {
		t.Fatalf("extract nonce: %v", err)
	}
	if n != 12 {
		t.Fatalf("embedded nonce = %d, want 12", n)
	}
	gp, err := extractETHGasPrice(raw)
	if err != nil {
		t.Fatalf("extract gasPrice: %v", err)
	}
	if gp != 1_000_000_000 {
		t.Fatalf("embedded gasPrice = %d, want 1e9", gp)
	}
	// 用嵌入的 nonce/gas 重建校验 tx，恢复签名者地址（证明签名与嵌入字段一致、私钥不出域）。
	checkTx := &UnsignedTx{Chain: ChainETH, To: "0x3535353535353535353535353535353535353535", Amount: amt(ChainETH, 1.0), Nonce: n, GasPriceWei: gp, GasLimit: 21000}
	rec, err := recoverETHAddressFromRaw(raw, checkTx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if rec != s.address {
		t.Fatalf("recovered address %s != signer address %s", rec, s.address)
	}
}

// TestRealSignerETHNonceIncrementsLocally 验证首次向节点取 Nonce 后本地递增，避免并发/未确认
// 期间重放碰撞：两次签名仅查询节点一次，嵌入 nonce 依次为 5、6。
func TestRealSignerETHNonceIncrementsLocally(t *testing.T) {
	st := &fakeETHState{nonce: 5}
	s, err := newRealSignerWithSource(HotWalletConfig{SignerKey: knownVectorPriv}, SignerSources{ETHState: st})
	if err != nil {
		t.Fatalf("newRealSignerWithSource: %v", err)
	}
	base := &UnsignedTx{Chain: ChainETH, To: "0x3535353535353535353535353535353535353535", Amount: amt(ChainETH, 1.0), GasPriceWei: 1e9, GasLimit: 21000}
	raw1, err := s.Sign(context.Background(), base)
	if err != nil {
		t.Fatalf("sign1: %v", err)
	}
	raw2, err := s.Sign(context.Background(), base)
	if err != nil {
		t.Fatalf("sign2: %v", err)
	}
	if st.nonceCalls != 1 {
		t.Fatalf("node Nonce should be queried once then cached, got %d calls", st.nonceCalls)
	}
	n1, _ := extractETHNonce(raw1)
	n2, _ := extractETHNonce(raw2)
	if n1 != 5 || n2 != 6 {
		t.Fatalf("expected local nonce increment 5 then 6, got %d and %d", n1, n2)
	}
}

// TestRPCWithdrawGatewayUsesRealSigner 验证网关提现路径经真实签名器「签名→SendRaw 广播」，
// 且广播的原始交易是真实可解析的 ETH raw tx（非 stub 标记串）。
func TestRPCWithdrawGatewayUsesRealSigner(t *testing.T) {
	fc := &fakeRPCClient{hash: "0xREALHASH"}
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Second),
		client:              fc,
		signer: NewSigner(HotWalletConfig{
			Enabled: true, SignerType: "hsm", SignerKey: knownVectorPriv,
			EthChainID: 1, EthGasPriceWei: 20000000000, EthGasLimit: 21000,
		}),
	}
	ev, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001),
		"0x3535353535353535353535353535353535353535", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fc.sentRaw {
		t.Fatalf("expected offline-signed raw tx broadcast via SendRaw")
	}
	if !strings.HasPrefix(fc.lastRaw, "0x") || len(fc.lastRaw) <= 2 {
		t.Fatalf("SendRaw got invalid raw: %q", fc.lastRaw)
	}
	// raw 必须是真实 RLP 列表（首字节 0xf8 或 0xb8… 视长度），且能恢复出签名者地址。
	if _, err := recoverETHAddressFromRaw(fc.lastRaw, &UnsignedTx{
		Chain: ChainETH, To: "0x3535353535353535353535353535353535353535",
		Amount: amt(ChainETH, 1.0), Nonce: 0, GasPriceWei: 20000000000, GasLimit: 21000, ChainID: 1,
	}); err != nil {
		t.Fatalf("broadcast raw tx not recoverable: %v", err)
	}
	if ev.TxHash != "0xREALHASH" {
		t.Fatalf("expected real tx hash from SendRaw, got %q", ev.TxHash)
	}
}

// ---------- 测试辅助 ----------

// recoverETHAddressFromRaw 从签名产生的 raw tx 反解 RLP，重构 compact 签名并恢复公钥，
// 派生地址；同时用与签名相同的字段重算摘要以验证（仅测试用）。
func recoverETHAddressFromRaw(raw string, tx *UnsignedTx) (string, error) {
	raw = strings.TrimPrefix(raw, "0x")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return "", err
	}
	items, err := splitRLPList(b)
	if err != nil {
		return "", err
	}
	if len(items) != 9 {
		return "", fmt.Errorf("expected 9 RLP fields, got %d", len(items))
	}
	// items: nonce, gasPrice, gasLimit, to, value, data, v, r, s
	v := new(big.Int).SetBytes(items[6])
	r := items[7]
	s := items[8]

	chainID := tx.ChainID
	recID := int(v.Int64()) - 27
	if chainID != 0 {
		recID = int(v.Int64()) - int(chainID*2+35)
	}
	compact := make([]byte, 65)
	compact[0] = byte(27 + recID)
	copy(compact[1:33], r)
	copy(compact[33:], s)

	// 重算摘要（与签名器一致）。
	toBytes, _ := parseETHAddress(tx.To)
	valueWei := amountToWei(tx.Amount)
	fields := [][]byte{
		rlpBigInt(big.NewInt(int64(tx.Nonce))),
		rlpBigInt(new(big.Int).SetUint64(tx.GasPriceWei)),
		rlpBigInt(new(big.Int).SetUint64(tx.GasLimit)),
		rlpAddress(toBytes),
		rlpBigInt(valueWei),
		rlpBytes(tx.Data),
	}
	if chainID != 0 {
		fields = append(fields,
			rlpBigInt(new(big.Int).SetUint64(chainID)),
			rlpBigInt(big.NewInt(0)),
			rlpBigInt(big.NewInt(0)),
		)
	}
	digest := keccak.Sum256(rlpEncodeList(fields))

	pub, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return "", fmt.Errorf("recover compact: %w", err)
	}
	return deriveETHAddress(pub), nil
}

// extractETHField 从已签 ETH raw tx 的 RLP 列表中取第 idx 个字段（大端整数）的值（仅测试用）。
func extractETHField(rawHex string, idx int) (uint64, error) {
	raw := strings.TrimPrefix(rawHex, "0x")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return 0, err
	}
	items, err := splitRLPList(b)
	if err != nil {
		return 0, err
	}
	if len(items) != 9 {
		return 0, fmt.Errorf("expected 9 RLP fields, got %d", len(items))
	}
	return new(big.Int).SetBytes(items[idx]).Uint64(), nil
}

func extractETHNonce(rawHex string) (uint64, error)   { return extractETHField(rawHex, 0) }
func extractETHGasPrice(rawHex string) (uint64, error) { return extractETHField(rawHex, 1) }

// splitRLPList 把 RLP 列表拆成各元素的原始 RLP 编码切片（仅支持元素为字符串的列表）。
func splitRLPList(b []byte) ([][]byte, error) {
	if len(b) == 0 || b[0] < 0xc0 {
		return nil, fmt.Errorf("not an RLP list")
	}
	var total, pos int
	if b[0] < 0xf8 {
		total = int(b[0]) - 0xc0
		pos = 1
	} else {
		n := int(b[0]) - 0xf7
		total = int(big.NewInt(0).SetBytes(b[1 : 1+n]).Int64())
		pos = 1 + n
	}
	end := pos + total
	if end > len(b) {
		return nil, fmt.Errorf("truncated list")
	}
	var items [][]byte
	for pos < end {
		p := b[pos]
		var elen, epos int
		switch {
		case p < 0x80:
			elen, epos = 1, pos
		case p < 0xb8:
			elen, epos = int(p)-0x80, pos+1
		default:
			n := int(p) - 0xb7
			elen = int(big.NewInt(0).SetBytes(b[pos+1 : pos+1+n]).Int64())
			epos = pos + 1 + n
		}
		items = append(items, b[epos:epos+elen])
		pos = epos + elen
	}
	return items, nil
}

// assertERC20TxFields 断言已签 ETH raw tx 是 ERC20 transfer 调用：to=代币合约、value=0、
// data=transfer(用户地址, 金额) 编码（selector 0xa9059cbb + pad32(to) + pad32(amount)）。
func assertERC20TxFields(t *testing.T, raw, contract, userAddr string, amt AssetAmount) {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(raw, "0x"))
	if err != nil {
		t.Fatalf("hex decode raw: %v", err)
	}
	items, err := splitRLPList(b)
	if err != nil {
		t.Fatalf("split RLP: %v", err)
	}
	if len(items) < 6 {
		t.Fatalf("need >=6 RLP items, got %d", len(items))
	}
	to, value, data := items[3], items[4], items[5]
	if !bytes.Equal(to, mustHex(strings.TrimPrefix(contract, "0x"))) {
		t.Fatalf("ERC20 to must be contract %s, got %x", contract, to)
	}
	if len(value) != 0 {
		t.Fatalf("ERC20 value must be 0, got %x", value)
	}
	want := encodeERC20Transfer(userAddr, amt)
	if !bytes.Equal(data, want) {
		t.Fatalf("ERC20 data mismatch:\n got %x\nwant %x", data, want)
	}
	if !bytes.HasPrefix(data, mustHex("a9059cbb")) {
		t.Fatalf("ERC20 data must start with transfer selector, got %x", data)
	}
}

// TestSignETHERC20 验证 ERC20 提现经 signETH 正确编码为 transfer 合约调用：链上 to=代币
// 合约、value=0、data=transfer(用户地址, 金额)（金额按代币 decimals=6 缩放后编码）。
func TestSignETHERC20(t *testing.T) {
	s, err := newRealSigner(HotWalletConfig{SignerKey: knownVectorPriv})
	if err != nil {
		t.Fatalf("newRealSigner: %v", err)
	}
	userAddr := "0x1111111111111111111111111111111111111111"
	contract := "0xdAC17F958D2ee523a2206206994597C13D831ec7"
	amt6 := AssetAmount{Value: big.NewInt(1500000), Decimals: 6} // 1.5 USDT
	tx := &UnsignedTx{
		Chain:           ChainETH,
		To:              userAddr,
		Amount:          amt6,
		Asset:           "USDT",
		ContractAddress: contract,
		Nonce:           9,
		GasPriceWei:     20000000000,
		GasLimit:        65000,
		ChainID:         1,
	}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	assertERC20TxFields(t, raw, contract, userAddr, amt6)
}
