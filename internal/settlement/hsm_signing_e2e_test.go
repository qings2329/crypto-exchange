package settlement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coldlar/crypto-exchange/internal/pkg/keccak"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// pad32 把大整数字节左补齐到 32 字节（ECDSA r/s 定长）。
func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	return append(make([]byte, 32-len(b)), b...)
}

// rlpContent 剥去单个 RLP 字符串项的长度前缀，返回其整数/字节内容（大端）。
func rlpContent(b []byte) []byte {
	fb := b[0]
	switch {
	case fb < 0x80:
		return b[:1]
	case fb <= 0xb7:
		return b[1 : 1+int(fb-0x80)]
	default:
		ll := int(fb - 0xb7)
		n := int(big.NewInt(0).SetBytes(b[1 : 1+ll]).Int64())
		return b[1+ll : 1+ll+n]
	}
}

// rlpSplitList 把顶层 RLP 列表拆成其子项的「原始编码字节切片」（含各自长度前缀），用于
// 独立重算待签摘要并验证签名归属（与以太坊节点做法一致）。仅覆盖本测试所需的列表形态。
func rlpSplitList(t *testing.T, b []byte) [][]byte {
	t.Helper()
	if len(b) == 0 || b[0] < 0xc0 {
		t.Fatalf("rlpSplitList: 期望列表，首字节 %#x", b[0])
	}
	var payload []byte
	if b[0] < 0xf8 {
		n := int(b[0] - 0xc0)
		payload = b[1 : 1+n]
	} else {
		ll := int(b[0] - 0xf7)
		n := int(big.NewInt(0).SetBytes(b[1 : 1+ll]).Int64())
		payload = b[1+ll : 1+ll+n]
	}
	var items [][]byte
	i := 0
	for i < len(payload) {
		fb := payload[i]
		var itemLen, headerLen int
		switch {
		case fb < 0x80:
			itemLen, headerLen = 1, 0
		case fb <= 0xb7:
			itemLen, headerLen = int(fb-0x80), 1
		case fb <= 0xbf:
			ll := int(fb - 0xb7)
			itemLen = int(big.NewInt(0).SetBytes(payload[i+1 : i+1+ll]).Int64())
			headerLen = 1 + ll
		case fb < 0xf8:
			itemLen, headerLen = int(fb-0xc0), 1
		default:
			ll := int(fb - 0xf7)
			itemLen = int(big.NewInt(0).SetBytes(payload[i+1 : i+1+ll]).Int64())
			headerLen = 1 + ll
		}
		items = append(items, payload[i:i+headerLen+itemLen])
		i += headerLen + itemLen
	}
	return items
}

// verifyETHSignature 独立验证一笔 ETH 离线签名：由 v 反推 recID 与 chainID，重算待签摘要
// （keccak(RLP([nonce, gasPrice, gasLimit, to, value, data, chainID, 0, 0]))，EIP-155），用
// 签名里的 (r,s) 反推公钥，断言其等于 want（HSM 导出的公钥）。这证明该交易确由该 HSM 密钥
// 签出且密码学有效（节点据此可恢复出发送方地址）。
func verifyETHSignature(t *testing.T, rawHex string, want *secp256k1.PublicKey) {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(rawHex, "0x"))
	if err != nil {
		t.Fatalf("ETH raw 非法: %v", err)
	}
	items := rlpSplitList(t, b)
	if len(items) != 9 {
		t.Fatalf("ETH tx 应为 9 元素 RLP 列表，got %d", len(items))
	}
	// items[7]/items[8] 是 r/s 的完整 RLP 编码（含长度前缀），须先剥前缀取整数内容。
	r := pad32(new(big.Int).SetBytes(rlpContent(items[7])).Bytes())
	s := pad32(new(big.Int).SetBytes(rlpContent(items[8])).Bytes())
	vInt := new(big.Int).SetBytes(rlpContent(items[6]))

	unsigned := [][]byte{items[0], items[1], items[2], items[3], items[4], items[5]}
	for recID := 0; recID <= 1; recID++ {
		// v = chainID*2 + 35 + recID  →  chainID = (v - 35 - recID) / 2
		chainID := new(big.Int).Sub(vInt, big.NewInt(int64(35+recID)))
		chainID.Div(chainID, big.NewInt(2))
		fields := append([][]byte{}, unsigned...)
		fields = append(fields, rlpBigInt(chainID), rlpBigInt(big.NewInt(0)), rlpBigInt(big.NewInt(0)))
		digest := keccak.Sum256(rlpEncodeList(fields))

		compact := make([]byte, 65)
		compact[0] = byte(27 + recID)
		copy(compact[1:33], r)
		copy(compact[33:], s)
		if p, _, err := ecdsa.RecoverCompact(compact, digest[:]); err == nil && p.IsEqual(want) {
			return
		}
	}
	t.Fatalf("ETH 签名无法恢复到期望公钥（HSM 密钥不匹配或签名无效）")
}

// verifyTronSignature 独立验证一笔 TRON 离线签名：txID 须等于 SHA256(raw_data)，且 65 字节
// 可恢复签名经 ecdsa.RecoverCompact(txID) 恢复出 want（HSM 导出公钥）；owner_address 须等于
// HSM 公钥派生的 TRON 地址。证明交易由该 HSM 密钥签出。
func verifyTronSignature(t *testing.T, rawJSON string, want *secp256k1.PublicKey) {
	t.Helper()
	b := parseTronBroadcast(t, rawJSON)
	rawData, err := hex.DecodeString(b.RawDataHex)
	if err != nil {
		t.Fatalf("raw_data_hex 非法: %v", err)
	}
	txID, err := hex.DecodeString(b.TxID)
	if err != nil || len(txID) != 32 {
		t.Fatalf("txID 非法: %v", err)
	}
	sum := sha256.Sum256(rawData)
	if !bytes.Equal(sum[:], txID) {
		t.Fatalf("TRON txID 不等于 SHA256(raw_data)")
	}
	if len(b.Signature) != 1 {
		t.Fatalf("期望 1 个签名，got %d", len(b.Signature))
	}
	sig, err := hex.DecodeString(b.Signature[0])
	if err != nil || len(sig) != 65 {
		t.Fatalf("TRON 签名须为 65 字节，got len=%d err=%v", len(sig), err)
	}
	p, _, err := ecdsa.RecoverCompact(sig, txID)
	if err != nil {
		t.Fatalf("RecoverCompact 失败: %v", err)
	}
	if !p.IsEqual(want) {
		t.Fatalf("TRON 签名未恢复到 HSM 公钥")
	}
	// owner_address 须等于 HSM 公钥派生的 TRON 地址。
	var val struct {
		OwnerAddress string `json:"owner_address"`
	}
	if err := json.Unmarshal(b.RawData.Contract[0].Parameter.Value, &val); err != nil {
		t.Fatalf("解析合约 value 失败: %v", err)
	}
	if val.OwnerAddress != deriveTronAddress(want) {
		t.Fatalf("owner_address 不符：got %s want %s", val.OwnerAddress, deriveTronAddress(want))
	}
}

// TestDeployRealHMSSignAndVerify 端到端验证「部署真实 HSM 并验证签名」：
// 启动真实 SigningService（持有真实 secp256k1 密钥），按 external+HSM 配置让网关自动注册
// 真实后端，对 ETH 与 TRON 签名，并独立恢复公钥验证签名确由该 HSM 密钥签出、地址对齐。
// 覆盖 remoteHSMKeySigner 的 {r,s} 与 DER 两种响应形态。
func TestDeployRealHMSSignAndVerify(t *testing.T) {
	const ethTo = "0x3535353535353535353535353535353535353535"
	tronTo := deriveTronAddress(parseSignerKeyOrDie(tronTestPrivRecipient).PubKey())
	ethTx := &UnsignedTx{Chain: ChainETH, To: ethTo, Amount: amt(ChainETH, 1.0), Nonce: 9, GasPriceWei: 20000000000, GasLimit: 21000, ChainID: 1}

	run := func(mode string) {
		svc := NewSigningService()
		pubHex := svc.PublicKeyHex()
		srv := httptest.NewServer(svc.Handler())
		defer srv.Close()
		keyID := "deployed-hsm-" + mode
		UnregisterExternalSigner(keyID)
		defer UnregisterExternalSigner(keyID)
		if err := svc.SetResponseMode(mode); err != nil {
			t.Fatalf("SetResponseMode: %v", err)
		}

		conf := HotWalletConfig{
			SignerKey:     keyID,
			SignerBackend: "external",
			HSM:           HSMConfig{Kind: "remote-http", Endpoint: srv.URL + "/sign", PublicKey: pubHex},
		}
		s, err := newRealSignerWithSource(conf, SignerSources{})
		if err != nil {
			t.Fatalf("[%s] 构造 realSigner 失败: %v", mode, err)
		}
		// 已按 HSMConfig 自动注册进全局注册表。
		if _, ok := lookupExternalSigner(keyID); !ok {
			t.Fatalf("[%s] external 后端未自动注册", mode)
		}
		// 签名器绑定的 ETH 地址须由 HSM 公钥派生。
		if s.address != deriveETHAddress(svc.Public()) {
			t.Fatalf("[%s] realSigner 地址与 HSM 公钥派生地址不符", mode)
		}

		// ETH 真实离线签名 + 独立验证归属 HSM 密钥。
		raw, err := s.Sign(context.Background(), ethTx)
		if err != nil {
			t.Fatalf("[%s] ETH 签名失败: %v", mode, err)
		}
		verifyETHSignature(t, raw, svc.Public())

		// TRON 真实离线签名（需参考区块源）+ 独立验证归属 HSM 密钥。
		s.tronState = &fakeTronState{blockNum: 12345, blockID: strings.Repeat("c0", 32), ts: 1600000000000}
		tronRaw, err := s.Sign(context.Background(), &UnsignedTx{Chain: ChainTRON, To: tronTo, Amount: amt(ChainTRON, 1.5)})
		if err != nil {
			t.Fatalf("[%s] TRON 签名失败: %v", mode, err)
		}
		verifyTronSignature(t, tronRaw, svc.Public())

		t.Logf("[%s] HSM_PUBLIC_KEY=%s ETH raw=%s", mode, pubHex, raw)
	}

	run("rs")
	run("der")
}
