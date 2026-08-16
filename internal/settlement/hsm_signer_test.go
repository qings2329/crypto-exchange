package settlement

import (
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
		Amount:      1.0,
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
		Amount: 1.0, Nonce: 9, GasPriceWei: 20000000000, GasLimit: 21000, ChainID: 1,
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

// TestRealSignerUnsupportedChain 验证 BTC/TRON 暂未实现，签名返回错误 → 网关回退节点侧广播。
func TestRealSignerUnsupportedChain(t *testing.T) {
	s, _ := newRealSigner(HotWalletConfig{SignerKey: knownVectorPriv})
	for _, ch := range []Chain{ChainBTC, ChainTRON} {
		if _, err := s.Sign(context.Background(), &UnsignedTx{Chain: ch, To: "x", Amount: 1}); err == nil {
			t.Fatalf("chain %s should not be signable yet", ch)
		}
	}
}

// TestRealSignerRequiresGasPrice 验证缺 gasPrice（tx 与配置均无）时签名失败 → 回退广播。
func TestRealSignerRequiresGasPrice(t *testing.T) {
	s, _ := newRealSigner(HotWalletConfig{SignerKey: knownVectorPriv}) // 未配 eth_gas_price_wei
	if _, err := s.Sign(context.Background(), &UnsignedTx{Chain: ChainETH, To: "0x3535", Amount: 1}); err == nil {
		t.Fatalf("expected error when gasPrice missing")
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
	ev, err := g.SubmitWithdraw(1, "ETH", ChainETH, 1.0, 0.001,
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
		Amount: 1.0, Nonce: 0, GasPriceWei: 20000000000, GasLimit: 21000, ChainID: 1,
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
