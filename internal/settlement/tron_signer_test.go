package settlement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// tronTestPrivRecipient / tronTestPrivContract 是测试用的第二、第三私钥（派生独立的 TRON 地址
// 作为收款方 / 合约地址），与 btcTestPriv 区分以构造真实的多方交易。
const (
	tronTestPrivRecipient = "4747474747474747474747474747474747474747474747474747474747474747"
	tronTestPrivContract  = "4848484848484848484848484848484848484848484848484848484848484848"
)

// fakeTronState 是 TRONStateSource 的内存假实现，返回固定的参考区块（无节点环境下验证
// 「真实参考区块→离线签名→广播」路径）。
type fakeTronState struct {
	blockNum int64
	blockID  string
	ts       int64
}

func (f *fakeTronState) NowBlock(ctx context.Context, chain Chain) (int64, string, int64, error) {
	return f.blockNum, f.blockID, f.ts, nil
}

// tronBroadcastJSON 是 signTRON 返回的广播 JSON 的最小解析结构（仅抽取校验所需字段）。
type tronBroadcastJSON struct {
	RawDataHex string `json:"raw_data_hex"`
	TxID       string `json:"txID"`
	Signature  []string `json:"signature"`
	RawData    struct {
		RefBlockBytes string `json:"ref_block_bytes"`
		RefBlockHash  string `json:"ref_block_hash"`
		Expiration    int64  `json:"expiration"`
		Timestamp     int64  `json:"timestamp"`
		FeeLimit      int64  `json:"fee_limit"`
		Contract      []struct {
			Parameter struct {
				Value   json.RawMessage `json:"value"`
				TypeURL string          `json:"type_url"`
			} `json:"parameter"`
			Type string `json:"type"`
		} `json:"contract"`
	} `json:"raw_data"`
}

// parseTronBroadcast 解析 signTRON 返回的广播 JSON。
func parseTronBroadcast(t *testing.T, raw string) tronBroadcastJSON {
	t.Helper()
	var b tronBroadcastJSON
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("解析 TRON 广播 JSON 失败: %v", err)
	}
	return b
}

// TestRealSignerTRONTransferContract 验证 TRX 原生转账（TransferContract）离线签名：
// ① raw_data_hex 单 SHA256 重算等于 txID；② 65 字节签名可恢复出签名者公钥；③ JSON 内
// owner_address 与金额正确。私钥不出域（软件后端）。
func TestRealSignerTRONTransferContract(t *testing.T) {
	priv := btcTestKey(t)
	state := &fakeTronState{
		blockNum: 12345,
		blockID:  strings.Repeat("c0", 32), // 32 字节区块哈希 hex
		ts:       1600000000000,
	}
	s := &realSigner{key: &softwareKeySigner{priv: priv}, tronState: state}

	to, err := tronAddressToBytes(deriveTronAddress(parseSignerKeyOrDie(tronTestPrivRecipient).PubKey()))
	if err != nil {
		t.Fatalf("recipient address: %v", err)
	}
	toB58 := deriveTronAddress(parseSignerKeyOrDie(tronTestPrivRecipient).PubKey())

	raw, err := s.Sign(context.Background(), &UnsignedTx{Chain: ChainTRON, To: toB58, Amount: amt(ChainTRON, 1.5)})
	if err != nil {
		t.Fatalf("sign TRON transfer: %v", err)
	}
	b := parseTronBroadcast(t, raw)

	// ① txID = SHA256(raw_data_hex)
	rawData, err := hex.DecodeString(b.RawDataHex)
	if err != nil {
		t.Fatalf("raw_data_hex 非法: %v", err)
	}
	recomputed := sha256.Sum256(rawData)
	txID, _ := hex.DecodeString(b.TxID)
	if !bytes.Equal(recomputed[:], txID) {
		t.Fatalf("txID 不等于 SHA256(raw_data)：%x vs %x", recomputed[:], txID)
	}

	// ② 65 字节可恢复签名 → 恢复公钥，须等于签名者公钥。
	if len(b.Signature) != 1 {
		t.Fatalf("期望 1 个签名，got %d", len(b.Signature))
	}
	sig, err := hex.DecodeString(b.Signature[0])
	if err != nil || len(sig) != 65 {
		t.Fatalf("TRON 签名须为 65 字节，got len=%d err=%v", len(sig), err)
	}
	recPub, _, err := ecdsa.RecoverCompact(sig, recomputed[:])
	if err != nil {
		t.Fatalf("RecoverCompact 失败: %v", err)
	}
	if !recPub.IsEqual(priv.PubKey()) {
		t.Fatalf("恢复的签名者公钥与预期不符")
	}

	// ③ JSON 业务字段核对：owner_address 为本签名者、金额为 1.5 TRX = 1500000 sun。
	if b.RawData.Contract[0].Type != "TransferContract" {
		t.Fatalf("期望 TransferContract，got %s", b.RawData.Contract[0].Type)
	}
	var val tronTransferValue
	if err := json.Unmarshal(b.RawData.Contract[0].Parameter.Value, &val); err != nil {
		t.Fatalf("解析 TransferContract value: %v", err)
	}
	if val.OwnerAddress != deriveTronAddress(priv.PubKey()) {
		t.Fatalf("owner_address=%s 应为 %s", val.OwnerAddress, deriveTronAddress(priv.PubKey()))
	}
	if val.ToAddress != toB58 {
		t.Fatalf("to_address=%s 应为 %s", val.ToAddress, toB58)
	}
	if val.Amount != 1500000 {
		t.Fatalf("amount(sun)=%d 应为 1500000", val.Amount)
	}
	// ref_block_bytes/hash 须等于区块哈希的末 2 / 前 8 字节。
	if b.RawData.RefBlockBytes != "c0c0" {
		t.Fatalf("ref_block_bytes=%s 应为 c0c0", b.RawData.RefBlockBytes)
	}
	if b.RawData.RefBlockHash != strings.Repeat("c0", 8) {
		t.Fatalf("ref_block_hash=%s 应为 8×c0", b.RawData.RefBlockHash)
	}
	if b.RawData.Expiration != b.RawData.Timestamp+tronExpirationDeltaMs {
		t.Fatalf("expiration 须为 timestamp+%d", tronExpirationDeltaMs)
	}
	_ = to
}

// TestRealSignerTRONTriggerSmartContract 验证 TRC20 transfer 合约调用（TriggerSmartContract）：
// data 字段为 a9059cbb + 收款地址(20 字节 HASH160 右对齐) + uint256 金额，可被独立重算。
func TestRealSignerTRONTriggerSmartContract(t *testing.T) {
	priv := btcTestKey(t)
	state := &fakeTronState{blockNum: 999, blockID: strings.Repeat("ab", 32), ts: 1700000000000}
	s := &realSigner{key: &softwareKeySigner{priv: priv}, tronState: state}

	toKey := parseSignerKeyOrDie(tronTestPrivRecipient)
	contractKey := parseSignerKeyOrDie(tronTestPrivContract)
	toB58 := deriveTronAddress(toKey.PubKey())
	contractB58 := deriveTronAddress(contractKey.PubKey())

	raw, err := s.Sign(context.Background(), &UnsignedTx{
		Chain:          ChainTRON,
		To:             toB58,
		Amount:         amt(ChainTRON, 1.0), // 1 USDT（人类单位），应缩放为 1e6 基础单位
		ContractAddress: contractB58,
		FeeLimit:       100_000_000, // 100 TRX 等价 sun
	})
	if err != nil {
		t.Fatalf("sign TRON trigger: %v", err)
	}
	b := parseTronBroadcast(t, raw)

	// txID 重算一致。
	rawData, _ := hex.DecodeString(b.RawDataHex)
	recomputed := sha256.Sum256(rawData)
	txID, _ := hex.DecodeString(b.TxID)
	if !bytes.Equal(recomputed[:], txID) {
		t.Fatalf("txID 不等于 SHA256(raw_data)")
	}
	// 签名可恢复出签名者公钥。
	sig, _ := hex.DecodeString(b.Signature[0])
	if _, _, err := ecdsa.RecoverCompact(sig, recomputed[:]); err != nil {
		t.Fatalf("RecoverCompact 失败: %v", err)
	}

	// 合约类型与 data 字段校验。
	if b.RawData.Contract[0].Type != "TriggerSmartContract" {
		t.Fatalf("期望 TriggerSmartContract，got %s", b.RawData.Contract[0].Type)
	}
	var val tronTriggerValue
	if err := json.Unmarshal(b.RawData.Contract[0].Parameter.Value, &val); err != nil {
		t.Fatalf("解析 TriggerSmartContract value: %v", err)
	}
	if val.ContractAddress != contractB58 {
		t.Fatalf("contract_address=%s 应为 %s", val.ContractAddress, contractB58)
	}
	if val.CallValue != 0 {
		t.Fatalf("call_value 应为 0，got %d", val.CallValue)
	}
	if b.RawData.FeeLimit != 100_000_000 {
		t.Fatalf("fee_limit=%d 应为 100000000", b.RawData.FeeLimit)
	}

	// 独立重算 data：a9059cbb || addrWord(32) || amtWord(32)，地址右对齐。
	dataBytes, err := hex.DecodeString(val.Data)
	if err != nil || len(dataBytes) != 68 {
		t.Fatalf("data 应为 68 字节（4+32+32），got len=%d", len(dataBytes))
	}
	if hex.EncodeToString(dataBytes[:4]) != tronTRC20TransferSelector {
		t.Fatalf("选择器应为 %s，got %x", tronTRC20TransferSelector, dataBytes[:4])
	}
	addrWord := dataBytes[4:36]
	gotHash := addrWord[12:] // 20 字节 HASH160 右对齐
	wantHash := tronAddressBytes(toKey.PubKey())[1:]
	if !bytes.Equal(gotHash, wantHash) {
		t.Fatalf("data 内地址字不匹配：got %x want %x", gotHash, wantHash)
	}
	amt := new(big.Int).SetBytes(dataBytes[36:68])
	if amt.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("data 内金额应为 1e6（1 USDT 缩放后），got %s", amt)
	}
}

// TestRealSignerTRONRequiresState 验证未注入 TRONState 时，TRON 签名返回错误（fail-degraded
// 由网关回退节点侧签名广播）。
func TestRealSignerTRONRequiresState(t *testing.T) {
	priv := btcTestKey(t)
	s := &realSigner{key: &softwareKeySigner{priv: priv}} // 无 tronState
	_, err := s.Sign(context.Background(), &UnsignedTx{Chain: ChainTRON, To: "Txxx", Amount: amt(ChainTRON, 1)})
	if err == nil {
		t.Fatalf("期望 TRON 签名因缺少 TRONState 而失败")
	}
}

// TestNewWithdrawGatewayTRONUsesOfflineSigner 端到端验证 TRON 提经网关主路径走
// 「getnowblock 取参考区块 → 离线签名 → /wallet/broadcasttransaction 广播」，返回节点 txid；
// 不回退节点侧签名。
func TestNewWithdrawGatewayTRONUsesOfflineSigner(t *testing.T) {
	blockID := strings.Repeat("de", 32)
	var gotNowBlock, gotBroadcast bool
	var broadcastTxID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/wallet/getnowblock":
			gotNowBlock = true
			_, _ = w.Write([]byte(`{"blockID":"` + blockID + `","block_header":{"raw_data":{"number":555,"timestamp":1700000000000}}}`))
		case "/wallet/broadcasttransaction":
			gotBroadcast = true
			var body tronBroadcastJSON
			_ = json.NewDecoder(r.Body).Decode(&body)
			broadcastTxID = body.TxID
			// 回显节点给的 txid，独立校验 txID = SHA256(raw_data)。
			rawData, _ := hex.DecodeString(body.RawDataHex)
			exp := sha256.Sum256(rawData)
			if strings.EqualFold(body.TxID, hex.EncodeToString(exp[:])) {
				_, _ = w.Write([]byte(`{"result":true,"txid":"` + body.TxID + `"}`))
			} else {
				_, _ = w.Write([]byte(`{"result":false,"code":"SIGERROR","message":"txid mismatch"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"TRON": srv.URL},
		HotWallet: HotWalletConfig{
			Enabled: true, SignerType: "hsm", SignerKey: btcTestPriv,
		},
	})
	if _, ok := g.(*RPCWithdrawGateway); !ok {
		t.Fatalf("enabled gateway should be *RPCWithdrawGateway, got %T", g)
	}

	recipient := deriveTronAddress(parseSignerKeyOrDie(tronTestPrivRecipient).PubKey())
	// M4：经工厂装配且配置了离线签名器（enforceAuth=true），须先登记门控授权。
	g.(*RPCWithdrawGateway).AuthorizeWithdraw(1, "TRX", ChainTRON, amt(ChainTRON, 2.0), amt(ChainTRON, 0.001), recipient, "")
	ev, err := g.SubmitWithdraw(1, "TRX", ChainTRON, amt(ChainTRON, 2.0), amt(ChainTRON, 0.001), recipient, false)
	if err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if !gotNowBlock {
		t.Fatalf("期望调用 getnowblock 取参考区块")
	}
	if !gotBroadcast {
		t.Fatalf("期望调用 broadcasttransaction 广播已签交易")
	}
	if ev.TxHash != broadcastTxID || ev.TxHash == "" {
		t.Fatalf("期望广播返回的 txid %q，got %q", broadcastTxID, ev.TxHash)
	}
	// txid 须为 64 hex（32 字节 SHA256）。
	if len(ev.TxHash) != 64 {
		t.Fatalf("txid 长度异常: %q", ev.TxHash)
	}
}

// parseSignerKeyOrDie 测试辅助：解析 32 字节 hex 私钥，失败即终止。
func parseSignerKeyOrDie(h string) *secp256k1.PrivateKey {
	k, err := parseSignerKey(h)
	if err != nil {
		panic(err)
	}
	return k
}
