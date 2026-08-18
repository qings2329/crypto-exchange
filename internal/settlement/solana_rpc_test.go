package settlement

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// solanaMockServer 起一个 JSON-RPC mock 服务，按 method 返回预置的 result 片段（已是合法 JSON），
// 验证扫描/广播/确认在「无真实节点」下经既有 JSONRPCClient 协议收发正确（接口缝测试）。
func solanaMockServer(t *testing.T, responses map[string]json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		resp, ok := responses[req.Method]
		if !ok {
			resp = json.RawMessage(`null`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(resp) + `}`))
	}))
}

// TestScanSOLNative 验证原生 SOL 充值扫描：getSignaturesForAddress + getTransaction 的
// pre/post Balances 差产出正确的 DepositEvent（金额、decimals=9、用户/链透传）。
func TestScanSOLNative(t *testing.T) {
	owner := randKey(t).PublicKey()
	payer := randKey(t).PublicKey()
	getTx := json.RawMessage(`{
		"meta": {"err": null, "preBalances": [1000000000, 0], "postBalances": [999000000, 1000000000]},
		"transaction": {"message": {"accountKeys": ["` + payer.String() + `", "` + owner.String() + `"]}}
	}`)
	srv := solanaMockServer(t, map[string]json.RawMessage{
		"getSignaturesForAddress": json.RawMessage(`[{"signature":"sigNative1","err":null}]`),
		"getTransaction":          getTx,
	})
	defer srv.Close()

	client := NewJSONRPCClient(map[string]string{string(ChainSOL): srv.URL})
	scanner := &JSONRPCDepositScanner{client: client}
	evs, err := scanner.scanSOL(context.Background(), DepositWatch{
		Chain: ChainSOL, Address: owner.String(), UserID: 7, Asset: "SOL",
	})
	if err != nil {
		t.Fatalf("scanSOL: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("期望 1 笔充值，实际 %d", len(evs))
	}
	ev := evs[0]
	if ev.TxHash != "sigNative1" || ev.UserID != 7 || ev.Chain != ChainSOL || ev.Asset != "SOL" {
		t.Fatalf("DepositEvent 透传字段错误: %+v", ev)
	}
	if ev.Amount.Decimals != 9 {
		t.Fatalf("SOL 充值 decimals 应为 9，实际 %d", ev.Amount.Decimals)
	}
	if got := ev.Amount.Value.Int64(); got != 1_000_000_000 {
		t.Fatalf("SOL 充值金额应为 1e9 lamports，实际 %d", got)
	}
	if ev.Address != owner.String() {
		t.Fatalf("DepositEvent.Address 应为 owner 钱包，实际 %s", ev.Address)
	}
}

// TestScanSOLSPL 验证 SPL/USDC 充值扫描：观察 owner 的 ATA，getTransaction 的 pre/post
// TokenBalances 差产出 USDC DepositEvent（decimals=6）。
func TestScanSOLSPL(t *testing.T) {
	owner := randKey(t).PublicKey()
	mintPub := solana.MustPublicKeyFromBase58(solanaUSDCContractMainnet)
	ata, _, err := solana.FindAssociatedTokenAddress(owner, mintPub)
	if err != nil {
		t.Fatalf("FindAssociatedTokenAddress: %v", err)
	}
	payer := randKey(t).PublicKey()
	getTx := json.RawMessage(`{
		"meta": {
			"err": null,
			"preBalances": [0, 0],
			"postBalances": [0, 0],
			"preTokenBalances": [{"accountIndex": 1, "mint": "` + mintPub.String() + `", "uiTokenAmount": {"amount": "0", "decimals": 6}}],
			"postTokenBalances": [{"accountIndex": 1, "mint": "` + mintPub.String() + `", "uiTokenAmount": {"amount": "1000000", "decimals": 6}}]
		},
		"transaction": {"message": {"accountKeys": ["` + payer.String() + `", "` + ata.String() + `"]}}
	}`)
	srv := solanaMockServer(t, map[string]json.RawMessage{
		"getSignaturesForAddress": json.RawMessage(`[{"signature":"sigSPL1","err":null}]`),
		"getTransaction":          getTx,
	})
	defer srv.Close()

	client := NewJSONRPCClient(map[string]string{string(ChainSOL): srv.URL})
	scanner := &JSONRPCDepositScanner{client: client}
	evs, err := scanner.scanSOL(context.Background(), DepositWatch{
		Chain: ChainSOL, Address: owner.String(), UserID: 9, Asset: "USDC",
	})
	if err != nil {
		t.Fatalf("scanSOL SPL: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("期望 1 笔 USDC 充值，实际 %d", len(evs))
	}
	ev := evs[0]
	if ev.TxHash != "sigSPL1" || ev.Asset != "USDC" || ev.Chain != ChainSOL {
		t.Fatalf("USDC DepositEvent 字段错误: %+v", ev)
	}
	if ev.Amount.Decimals != 6 {
		t.Fatalf("USDC 充值 decimals 应为 6，实际 %d", ev.Amount.Decimals)
	}
	if got := ev.Amount.Value.Int64(); got != 1_000_000 {
		t.Fatalf("USDC 充值金额应为 1e6，实际 %d", got)
	}
	// 充值地址应为 owner 钱包（而非 ATA），与观察配置一致。
	if ev.Address != owner.String() {
		t.Fatalf("USDC DepositEvent.Address 应为 owner 钱包，实际 %s", ev.Address)
	}
}

// TestScanSOLFailedTxSkipped 验证失败交易（meta.err 非空）不计充值。
func TestScanSOLFailedTxSkipped(t *testing.T) {
	owner := randKey(t).PublicKey()
	getTx := json.RawMessage(`{
		"meta": {"err": {"InstructionError": [0, "Custom", 1]}, "preBalances": [100, 0], "postBalances": [100, 0]},
		"transaction": {"message": {"accountKeys": ["` + owner.String() + `", "` + owner.String() + `"]}}
	}`)
	srv := solanaMockServer(t, map[string]json.RawMessage{
		"getSignaturesForAddress": json.RawMessage(`[{"signature":"sigFail","err":null}]`),
		"getTransaction":          getTx,
	})
	defer srv.Close()

	client := NewJSONRPCClient(map[string]string{string(ChainSOL): srv.URL})
	scanner := &JSONRPCDepositScanner{client: client}
	evs, err := scanner.scanSOL(context.Background(), DepositWatch{
		Chain: ChainSOL, Address: owner.String(), UserID: 1, Asset: "SOL",
	})
	if err != nil {
		t.Fatalf("scanSOL: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("失败交易不应产出充值，实际 %d", len(evs))
	}
}

// TestSendRawSOL 验证 sendTransaction 经 JSONRPCClient 发出并取回 base58 签名作为 TxHash。
func TestSendRawSOL(t *testing.T) {
	srv := solanaMockServer(t, map[string]json.RawMessage{
		"sendTransaction": json.RawMessage(`"11111111111111111111111111111111"`),
	})
	defer srv.Close()
	client := NewJSONRPCClient(map[string]string{string(ChainSOL): srv.URL})
	h, err := client.SendRaw(context.Background(), ChainSOL, "11111111111111111111111111111111")
	if err != nil {
		t.Fatalf("SendRaw SOL: %v", err)
	}
	if h != "11111111111111111111111111111111" {
		t.Fatalf("SendRaw 返回签名错误: %s", h)
	}
}

// TestConfirmationsSOL 验证 getSignatureStatuses 解析确认数，以及 null/负值钳制为 0（#10）。
func TestConfirmationsSOL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"confirmed", `{"value":[{"confirmations":5,"confirmationStatus":"confirmed","err":null}]}`, 5},
		{"null-clamp", `{"value":[{"confirmations":null,"confirmationStatus":"processed","err":null}]}`, 0},
		{"negative-clamp", `{"value":[{"confirmations":-3,"confirmationStatus":"finalized","err":null}]}`, 0},
		{"empty", `{"value":[]}`, 0},
		{"tx-error", `{"value":[{"confirmations":10,"confirmationStatus":"finalized","err":{"InstructionError":[0,"Custom",1]}}]}`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := solanaMockServer(t, map[string]json.RawMessage{
				"getSignatureStatuses": json.RawMessage(c.raw),
			})
			defer srv.Close()
			client := NewJSONRPCClient(map[string]string{string(ChainSOL): srv.URL})
			got, err := client.Confirmations(context.Background(), ChainSOL, "sigX")
			if err != nil {
				t.Fatalf("Confirmations: %v", err)
			}
			if got != c.want {
				t.Fatalf("Confirmations 期望 %d，实际 %d", c.want, got)
			}
		})
	}
}

// TestSolanaRecentBlockHash 验证 getLatestBlockhash 的 base58 解码路径（生产交易构造所需）。
func TestSolanaRecentBlockHash(t *testing.T) {
	bh := make([]byte, 32)
	bh[0] = 0x07 // 非零，便于断言往返
	enc := base58Encode(bh)
	srv := solanaMockServer(t, map[string]json.RawMessage{
		"getLatestBlockhash": json.RawMessage(`{"value":{"blockhash":"` + enc + `"}}`),
	})
	defer srv.Close()
	client := NewJSONRPCClient(map[string]string{string(ChainSOL): srv.URL})
	h, err := client.SolanaRecentBlockHash(context.Background())
	if err != nil {
		t.Fatalf("SolanaRecentBlockHash: %v", err)
	}
	if h[0] != 0x07 {
		t.Fatalf("区块哈希首字节应为 0x07，实际 0x%02x", h[0])
	}
}

// TestScanSOLBadResponseFailDegraded 验证节点返回非法 JSON 时扫描报错（上层 scanOnce 据此
// skip，fail-degraded 不阻断其它链）。
func TestScanSOLBadResponseFailDegraded(t *testing.T) {
	srv := solanaMockServer(t, map[string]json.RawMessage{
		"getSignaturesForAddress": json.RawMessage(`"not-an-array"`),
	})
	defer srv.Close()
	client := NewJSONRPCClient(map[string]string{string(ChainSOL): srv.URL})
	scanner := &JSONRPCDepositScanner{client: client}
	_, err := scanner.scanSOL(context.Background(), DepositWatch{
		Chain: ChainSOL, Address: randKey(t).PublicKey().String(), UserID: 1, Asset: "SOL",
	})
	if err == nil {
		t.Fatal("非法响应应返回错误（fail-degraded skip）")
	}
}
