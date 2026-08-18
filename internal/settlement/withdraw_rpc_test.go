package settlement

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeRPCClient 是 ChainRPCClient 的内存假实现，用于无节点环境下验证「真实哈希注入」路径。
// 同时记录走的是离线签名广播(SendRaw)还是节点侧签名广播(Broadcast)。
type fakeRPCClient struct {
	hash    string
	err     error
	sentRaw bool
	lastRaw string
}

func (f *fakeRPCClient) Broadcast(ctx context.Context, chain Chain, to string, amount AssetAmount) (string, error) {
	return f.hash, f.err
}

func (f *fakeRPCClient) SendRaw(ctx context.Context, chain Chain, rawHex string) (string, error) {
	f.sentRaw = true
	f.lastRaw = rawHex
	return f.hash, f.err
}

// BroadcastERC20 仅为满足 ChainRPCClient 接口（fakeRPCClient 用于非 ERC20 提现的网关测试，
// 不会触发此路径）。ERC20 提现的端到端测试使用真实 *JSONRPCClient + httptest。
func (f *fakeRPCClient) BroadcastERC20(ctx context.Context, chain Chain, contract, to string, amount AssetAmount) (string, error) {
	return f.hash, f.err
}

// fakeSigner 是 Signer 的内存假实现，记录被调用并返回确定性 raw。
type fakeSigner struct {
	called bool
	raw    string
}

func (f *fakeSigner) Sign(ctx context.Context, tx *UnsignedTx) (string, error) {
	f.called = true
	f.raw = "0xREALRAW"
	return f.raw, nil
}

// fakeConfirmSource 是 ConfirmSource 的内存假实现，用于无节点环境下验证「真实确认数→状态机推进」。
type fakeConfirmSource struct {
	conf int
	err  error
}

func (f *fakeConfirmSource) Confirmations(ctx context.Context, chain Chain, txHash string) (int, error) {
	return f.conf, f.err
}

// TestNewWithdrawGatewayDisabledReturnsMock 验证未启用 RPC 时回退到模拟网关，
// 提现受理返回本地生成的模拟 TxHash，行为与改动前一致（零回归）。
func TestNewWithdrawGatewayDisabledReturnsMock(t *testing.T) {
	g := NewWithdrawGateway(ChainRPCConfig{Enabled: false})
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 100), amt(ChainETH, 0.1), "0xabc", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev == nil || ev.TxHash == "" {
		t.Fatalf("expected non-empty simulated tx hash")
	}
	if ev.TxHash == "0xREAL" {
		t.Fatalf("disabled gateway must not use real hash")
	}
}

// TestRPCWithdrawGatewayInjectsRealHash 验证配置了 RPC 客户端时，广播成功后内部事件
// 采用节点返回的真实 TxHash（链上记录与内部事件一致）。
func TestRPCWithdrawGatewayInjectsRealHash(t *testing.T) {
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Second),
		client:              &fakeRPCClient{hash: "0xREALHASH"},
	}
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 100), amt(ChainETH, 0.1), "0xabc", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.TxHash != "0xREALHASH" {
		t.Fatalf("expected real tx hash injected, got %q", ev.TxHash)
	}
	// 其余字段仍由内部状态机填充。
	if ev.Required != 2 || ev.Status != WithdrawPending {
		t.Fatalf("unexpected event fields: %+v", ev)
	}
}

// TestRPCWithdrawGatewayFallsBackOnClientError 验证 RPC 不可达时自动回退模拟广播，
// 保证无外部节点也能运行（fail-degraded）。
func TestRPCWithdrawGatewayFallsBackOnClientError(t *testing.T) {
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Second),
		client:              &fakeRPCClient{err: errors.New("rpc unreachable")},
	}
	ev, err := g.SubmitWithdraw(1, "USDT", ChainBTC, amt(ChainBTC, 0.5), amt(ChainBTC, 0.0005), "bc1xyz", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.TxHash == "" || ev.TxHash == "0xREALHASH" {
		t.Fatalf("expected simulated fallback hash, got %q", ev.TxHash)
	}
}

// TestNewWithdrawGatewayEnabledUsesRPC 验证启用且配置端点时返回 RPC 网关（真实广播路径），
// 且工厂产出的实例满足 WithdrawGateway 契约。
func TestNewWithdrawGatewayEnabledUsesRPC(t *testing.T) {
	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": "http://127.0.0.1:8545"},
	})
	var _ WithdrawGateway = g // 编译期契约检查
	_, ok := g.(*RPCWithdrawGateway)
	if !ok {
		t.Fatalf("enabled gateway should be *RPCWithdrawGateway, got %T", g)
	}
}

// TestRPCWithdrawGatewayUsesRealConfirmations 验证配置了确认源时，广播后确认数按节点
// 返回的真实确认数推进（而非模拟 +1），达标即置 Credited（真实区块确认轮询路径）。
func TestRPCWithdrawGatewayUsesRealConfirmations(t *testing.T) {
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Second),
		client:              &fakeRPCClient{hash: "0xREAL"},
	}
	g.MockWithdrawGateway.confirmSource = &fakeConfirmSource{conf: 5} // 节点返回 5 个确认
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 100), amt(ChainETH, 0.1), "0xabc", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g.Tick() // Pending -> Broadcasting（确认=1，状态转移）
	g.Tick() // Broadcasting -> 真实确认数 5 >= 2 -> Credited
	// 重新取事件核对。
	for _, e := range g.WithdrawHistory() {
		if e.TxHash == ev.TxHash {
			if e.Status != WithdrawCredited {
				t.Fatalf("expected Credited, got %s", e.Status)
			}
			if e.Confirmations != 5 {
				t.Fatalf("expected real confirmations 5, got %d", e.Confirmations)
			}
		}
	}
}

// TestRPCWithdrawGatewayFallsBackOnConfirmError 验证确认源不可达时自动回退模拟 +1，
// 行为与原 Mock 一致（fail-degraded）。
func TestRPCWithdrawGatewayFallsBackOnConfirmError(t *testing.T) {
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Second),
		client:              &fakeRPCClient{hash: "0xREAL"},
	}
	g.MockWithdrawGateway.confirmSource = &fakeConfirmSource{err: errors.New("node unreachable")}
	ev, err := g.SubmitWithdraw(1, "USDT", ChainBTC, amt(ChainBTC, 0.5), amt(ChainBTC, 0.0005), "bc1xyz", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g.Tick() // Pending -> Broadcasting（确认=1）
	g.Tick() // Broadcasting -> 回退模拟 1+1=2 -> Credited
	for _, e := range g.WithdrawHistory() {
		if e.TxHash == ev.TxHash {
			if e.Status != WithdrawCredited {
				t.Fatalf("expected Credited, got %s", e.Status)
			}
			if e.Confirmations != 2 {
				t.Fatalf("expected simulated confirmations 2, got %d", e.Confirmations)
			}
		}
	}
}

// TestJSONRPCClientConfirmationsETH 用 httptest 模拟节点：eth_blockNumber=0x10、交易
// blockNumber=0x9，验证确认数 = 16-9+1 = 8。
func TestJSONRPCClientConfirmationsETH(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "eth_blockNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`))
		case "eth_getTransactionByHash":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"blockNumber":"0x9"}}`))
		}
	}))
	defer srv.Close()

	c := NewJSONRPCClient(map[string]string{"ETH": srv.URL})
	conf, err := c.Confirmations(context.Background(), ChainETH, "0xdead")
	if err != nil {
		t.Fatalf("confirmations failed: %v", err)
	}
	if conf != 8 {
		t.Fatalf("expected 8 confirmations (16-9+1), got %d", conf)
	}
}

// TestJSONRPCClientConfirmationsBTC 用 httptest 模拟节点 getrawtransaction，验证直接取 confirmations 字段。
func TestJSONRPCClientConfirmationsBTC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"confirmations":4}}`))
	}))
	defer srv.Close()

	c := NewJSONRPCClient(map[string]string{"BTC": srv.URL})
	conf, err := c.Confirmations(context.Background(), ChainBTC, "txid")
	if err != nil {
		t.Fatalf("confirmations failed: %v", err)
	}
	if conf != 4 {
		t.Fatalf("expected 4 confirmations, got %d", conf)
	}
}

// TestJSONRPCClientConfirmationsTRON 用 httptest 模拟 TronGrid：链头 /v1/blocks number=100、
// 交易 /v1/transactions/{id} blockNumber=95，验证确认数 = 100-95+1 = 6。
func TestJSONRPCClientConfirmationsTRON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/blocks"):
			_, _ = w.Write([]byte(`{"data":[{"number":100}]}`))
		case strings.HasPrefix(r.URL.Path, "/v1/transactions/"):
			_, _ = w.Write([]byte(`{"data":[{"blockNumber":95}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewJSONRPCClient(map[string]string{"TRON": srv.URL})
	conf, err := c.Confirmations(context.Background(), ChainTRON, "deadbeef")
	if err != nil {
		t.Fatalf("tron confirmations failed: %v", err)
	}
	if conf != 6 {
		t.Fatalf("expected 6 confirmations (100-95+1), got %d", conf)
	}
}

// TestJSONRPCClientConfirmationsClampsNegative 模拟链重组/节点时间偏差使交易所在区块高于链头
// （差值+1 为负），验证确认数被钳制为 0（≡尚未确认），避免状态机误判已确认提前入账（#10）。
func TestJSONRPCClientConfirmationsClampsNegative(t *testing.T) {
	tests := []struct {
		name  string
		chain Chain
		// handler 返回：链头区块号、交易所在区块号。
		headHex, txHex   string // ETH/TRON 用十六进制；BTC 走 confirmations 字段
		btcConfirmations int    // BTC 节点直接给的 confirmations（可为负）
	}{
		{name: "ETH reorg", chain: ChainETH, headHex: "0x9", txHex: "0x10"},
		{name: "TRON reorg", chain: ChainTRON, headHex: "0x5f", txHex: "0x64"}, // 95 vs 100
		{name: "BTC negative", chain: ChainBTC, btcConfirmations: -3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var req struct {
					Method string `json:"method"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				switch {
				case strings.HasPrefix(r.URL.Path, "/v1/blocks"):
					_, _ = w.Write([]byte(`{"data":[{"number":` + parseHexToDec(tc.headHex) + `}]}`))
				case strings.HasPrefix(r.URL.Path, "/v1/transactions/"):
					_, _ = w.Write([]byte(`{"data":[{"blockNumber":` + parseHexToDec(tc.txHex) + `}]}`))
				case req.Method == "eth_blockNumber":
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + tc.headHex + `"}`))
				case req.Method == "eth_getTransactionByHash":
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"blockNumber":"` + tc.txHex + `"}}`))
				case req.Method == "getrawtransaction":
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"confirmations":` + strconv.Itoa(tc.btcConfirmations) + `}}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := NewJSONRPCClient(map[string]string{string(tc.chain): srv.URL})
			conf, err := c.Confirmations(context.Background(), tc.chain, "txhash")
			if err != nil {
				t.Fatalf("confirmations failed: %v", err)
			}
			if conf != 0 {
				t.Fatalf("expected negative conf clamped to 0, got %d", conf)
			}
		})
	}
}

// parseHexToDec 把 "0x.." 转为十进制字符串（仅测试用），供 TronGrid 十进制区块号响应构造。
func parseHexToDec(h string) string {
	if n, err := strconv.ParseInt(strings.TrimPrefix(h, "0x"), 16, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return "0"
}

// TestRPCWithdrawGatewayUsesOfflineSigner 验证配置了签名器时，提现走「离线签名→SendRaw
// 广播原始交易」路径（私钥不出域），返回节点给的真实 TxHash；不经过节点侧 Broadcast。
func TestRPCWithdrawGatewayUsesOfflineSigner(t *testing.T) {
	fc := &fakeRPCClient{hash: "0xREALHASH"}
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Second),
		client:              fc,
		signer:              &fakeSigner{},
	}
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 100), amt(ChainETH, 0.1), "0xabc", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fc.sentRaw {
		t.Fatalf("expected offline-signed raw tx broadcast via SendRaw")
	}
	if fc.lastRaw != "0xREALRAW" {
		t.Fatalf("SendRaw got wrong raw: %q", fc.lastRaw)
	}
	if ev.TxHash != "0xREALHASH" {
		t.Fatalf("expected real tx hash from SendRaw, got %q", ev.TxHash)
	}
}

// TestRPCWithdrawGatewaySignerErrorFallsBackToNode 验证离线签名失败时自动回退节点侧签名
// 广播（fail-degraded）：签名器返回错误 → 走 Broadcast 路径取回哈希。
func TestRPCWithdrawGatewaySignerErrorFallsBackToNode(t *testing.T) {
	fc := &fakeRPCClient{hash: "0xNODEHASH"}
	badSigner := &fakeSignerErr{}
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Second),
		client:              fc,
		signer:              badSigner,
	}
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 100), amt(ChainETH, 0.1), "0xabc", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fc.sentRaw {
		t.Fatalf("signer errored; must not broadcast raw tx")
	}
	if ev.TxHash != "0xNODEHASH" {
		t.Fatalf("expected node-side hash after signer failure, got %q", ev.TxHash)
	}
}

// fakeSignerErr 是始终返回错误的签名器（验证 fail-degraded 回退）。
type fakeSignerErr struct{}

func (f *fakeSignerErr) Sign(ctx context.Context, tx *UnsignedTx) (string, error) {
	return "", errors.New("signer unavailable")
}

// TestNewWithdrawGatewayETHUsesNodeNonceGas 验证 ETH 提现经网关主路径向节点查询真实
// Nonce（eth_getTransactionCount）/ Gas 价（eth_gasPrice），嵌入签名后以 eth_sendRawTransaction
// 广播（私钥不出域）。未显式配置 Nonce/Gas 时不再使用过期/默认 0 Nonce。
func TestNewWithdrawGatewayETHUsesNodeNonceGas(t *testing.T) {
	var gotNonce, gotGas bool
	var rawSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "eth_getTransactionCount":
			gotNonce = true
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x9"}`))
		case "eth_gasPrice":
			gotGas = true
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x3b9aca00"}`)) // 1 gwei
		case "eth_sendRawTransaction":
			if len(req.Params) > 0 {
				rawSeen = strings.Trim(string(req.Params[0]), `"`)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xethhash"}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
		}
	}))
	defer srv.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": srv.URL},
		HotWallet: HotWalletConfig{
			Enabled: true, SignerType: "hsm", SignerKey: knownVectorPriv,
			EthChainID: 1, EthGasLimit: 21000,
		},
	})
	if _, ok := g.(*RPCWithdrawGateway); !ok {
		t.Fatalf("enabled gateway should be *RPCWithdrawGateway, got %T", g)
	}

	// 取签名者地址用于恢复校验（与网关内签名器同私钥）。
	sig, _ := newRealSignerWithSource(HotWalletConfig{SignerKey: knownVectorPriv}, SignerSources{})
	to := "0x3535353535353535353535353535353535353535"
	ev, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), to, false)
	if err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if !gotNonce || !gotGas {
		t.Fatalf("expected node nonce + gas queries, got nonce=%v gas=%v", gotNonce, gotGas)
	}
	if rawSeen == "" {
		t.Fatalf("eth_sendRawTransaction did not receive raw")
	}
	// nonce 9 应被嵌入 raw，且签名可恢复出签名者地址（与嵌入字段一致、私钥不出域）。
	n, err := extractETHNonce(rawSeen)
	if err != nil {
		t.Fatalf("extract nonce: %v", err)
	}
	if n != 9 {
		t.Fatalf("embedded nonce = %d, want 9", n)
	}
	gp, _ := extractETHGasPrice(rawSeen)
	checkTx := &UnsignedTx{Chain: ChainETH, To: to, Amount: amt(ChainETH, 1.0), Nonce: n, GasPriceWei: gp, GasLimit: 21000, ChainID: 1}
	rec, err := recoverETHAddressFromRaw(rawSeen, checkTx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if rec != sig.address {
		t.Fatalf("recovered address %s != signer address %s", rec, sig.address)
	}
	if ev.TxHash != "0xethhash" {
		t.Fatalf("expected real tx hash from SendRaw, got %q", ev.TxHash)
	}
}

// TestNewWithdrawGatewayETHERC20UsesOfflineSigner 验证 ERC20（USDT）提现经网关主路径走
// 「离线签名 → SendRaw 广播」：构造 transfer 合约调用（to=合约、value=0、data=transfer 编码）。
func TestNewWithdrawGatewayETHERC20UsesOfflineSigner(t *testing.T) {
	var rawSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "eth_sendRawTransaction":
			if len(req.Params) > 0 {
				rawSeen = strings.Trim(string(req.Params[0]), `"`)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xethhash"}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
		}
	}))
	defer srv.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": srv.URL},
		HotWallet: HotWalletConfig{
			Enabled: true, SignerType: "hsm", SignerKey: knownVectorPriv,
			EthChainID: 1, EthGasPriceWei: 20000000000, EthGasLimit: 65000,
		},
		ERC20Tokens: map[string]ERC20TokenInfo{
			"USDT": {Contract: "0xdAC17F958D2ee523a2206206994597C13D831ec7", Decimals: 6},
		},
	})
	rg, ok := g.(*RPCWithdrawGateway)
	if !ok {
		t.Fatalf("enabled gateway should be *RPCWithdrawGateway, got %T", g)
	}
	if _, ok := rg.erc20["USDT"]; !ok {
		t.Fatalf("ERC20 token registry not loaded into gateway")
	}
	userAddr := "0x1111111111111111111111111111111111111111"
	// 1.5 USDT 经网关内部 ToDecimals(6) → 1500000 最小单位。
	ev, err := rg.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 1.5), amt(ChainETH, 0.001), userAddr, false)
	if err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if rawSeen == "" {
		t.Fatalf("eth_sendRawTransaction did not receive raw (ERC20 should go through offline signer)")
	}
	if ev.TxHash != "0xethhash" {
		t.Fatalf("expected real tx hash from SendRaw, got %q", ev.TxHash)
	}
	// 断言 raw 是 ERC20 transfer 调用（to=合约、value=0、data=transfer 编码，金额按 6 decimals 缩放）。
	assertERC20TxFields(t, rawSeen, "0xdAC17F958D2ee523a2206206994597C13D831ec7", userAddr, amt(ChainETH, 1.5).ToDecimals(6))
}

// TestNewWithdrawGatewayBTCUsesOfflineSignerMainPath 验证 BTC 提现经网关主路径走
// 「真实 UTXO 拉取 → 离线签名 → SendRaw 广播」，不再回退节点侧 sendtoaddress：
// 配置 RPC 端点 + hsm 签名器后，网关先用 listunspent 取 UTXO，做 SIGHASH_ALL 真实签名，
// 再 sendrawtransaction 广播，返回节点给的真实 TxHash。
func TestNewWithdrawGatewayBTCUsesOfflineSignerMainPath(t *testing.T) {
	priv := btcTestKey(t)
	ownAddr := deriveP2WPKHAddress(priv.PubKey())
	ownScript, err := addressToScriptPubKey(ownAddr) // 同私钥派生，可被签名器花费
	if err != nil {
		t.Fatalf("derive own script: %v", err)
	}
	ownScriptHex := hex.EncodeToString(ownScript)

	// 节点同时响应 listunspent（UTXO 源）与 sendrawtransaction（主路径广播）。
	var sentRaw bool
	var rawSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string              `json:"method"`
			Params []json.RawMessage   `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "listunspent":
			res := `[{"txid":"` + strings.Repeat("a", 64) + `","vout":0,"amount":1.0,"scriptPubKey":"` + ownScriptHex + `"}]`
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + res + `}`))
		case "sendrawtransaction":
			sentRaw = true
			if len(req.Params) > 0 {
				rawSeen = strings.Trim(string(req.Params[0]), `"`)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"btctxhash"}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
		}
	}))
	defer srv.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"BTC": srv.URL},
		HotWallet: HotWalletConfig{
			Enabled:    true,
			SignerType: "hsm",
			SignerKey:  btcTestPriv,
		},
	})
	if _, ok := g.(*RPCWithdrawGateway); !ok {
		t.Fatalf("enabled gateway should be *RPCWithdrawGateway, got %T", g)
	}

	ev, err := g.SubmitWithdraw(1, "BTC", ChainBTC, amt(ChainBTC, 0.5), amt(ChainBTC, 0.0005), deriveP2WPKHAddress(priv.PubKey()), false)
	if err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if !sentRaw {
		t.Fatalf("BTC 应走 SendRaw（离线签名后广播）主路径，而非节点侧 sendtoaddress")
	}
	if ev.TxHash != "btctxhash" {
		t.Fatalf("expected real tx hash from SendRaw, got %q", ev.TxHash)
	}
	if rawSeen == "" {
		t.Fatalf("SendRaw 未收到 raw hex")
	}
	// 复用独立验证辅助：解析 raw、重算 SIGHASH 摘要交叉比对并校验 ECDSA 签名自洽 + 金额守恒。
	utxos := []UTXO{{TxID: strings.Repeat("a", 64), Vout: 0, Amount: 1.0, ScriptPubKey: ownScriptHex}}
	verifyBTCSignatures(t, rawSeen, utxos, priv.PubKey().SerializeCompressed())
	verifyBTCValueConservation(t, rawSeen, utxos, amt(ChainBTC, 0.5))
}

// failSigner 是 Signer 的内存假实现，Sign 始终返回错误，用于验证离线签名失败时的降级告警（G2）。
type failSigner struct{ err error }

func (f *failSigner) Sign(ctx context.Context, tx *UnsignedTx) (string, error) { return "", f.err }

// emptyRawSigner 是 Signer 的内存假实现，Sign 返回 ("", nil)（空 raw 无错误），用于验证
// 离线签名器有意不离线签时的静默降级被 WARN 暴露（G2 补全项）。
type emptyRawSigner struct{}

func (e *emptyRawSigner) Sign(ctx context.Context, tx *UnsignedTx) (string, error) { return "", nil }

// TestSubmitWithdrawLogsERC20RoutedAndBroadcast 锁定 G1/G4：ERC20 提现须输出「routed」路由日志
// 与「offline-signed broadcast OK」成功日志（含真实 txHash），便于对账与审计。
func TestSubmitWithdrawLogsERC20RoutedAndBroadcast(t *testing.T) {
	var rawSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []json.RawMessage
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "eth_sendRawTransaction" && len(req.Params) > 0 {
			rawSeen = strings.Trim(string(req.Params[0]), `"`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xethhash"}`))
	}))
	defer srv.Close()
	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": srv.URL},
		HotWallet: HotWalletConfig{Enabled: true, SignerType: "hsm", SignerKey: knownVectorPriv, EthChainID: 1, EthGasPriceWei: 20000000000, EthGasLimit: 65000},
		ERC20Tokens: map[string]ERC20TokenInfo{"USDT": {Contract: "0xdAC17F958D2ee523a2206206994597C13D831ec7", Decimals: 6}},
	}).(*RPCWithdrawGateway)
	buf, restore := captureLog(t)
	defer restore()
	if _, err := g.SubmitWithdraw(1, "USDT", ChainETH, amt(ChainETH, 1.5), amt(ChainETH, 0.001), "0x1111111111111111111111111111111111111111", false); err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if rawSeen == "" {
		t.Fatal("eth_sendRawTransaction 未收到 raw")
	}
	if !strings.Contains(buf.String(), "ERC20 withdraw routed") {
		t.Fatalf("期望 G1 ERC20 routed 日志，实际: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "withdraw broadcast OK (offline-signed)") {
		t.Fatalf("期望 G4 离线签名广播成功日志，实际: %q", buf.String())
	}
}

// TestSubmitWithdrawLogsERC20Misconfig 锁定 G1 高危自检：ETH 链上非原生资产未命中 ERC20 注册表时，
// 须输出 ERROR（将作为原生 ETH 广播，资金错路），而非静默误转。
func TestSubmitWithdrawLogsERC20Misconfig(t *testing.T) {
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Hour),
		client:              &fakeRPCClient{hash: "0xnative"},
		signer:              nil,
		erc20:               map[string]ERC20TokenInfo{"USDT": {Contract: "0xdAC17F958D2ee523a2206206994597C13D831ec7", Decimals: 6}},
	}
	buf, restore := captureLog(t)
	defer restore()
	if _, err := g.SubmitWithdraw(1, "USDC", ChainETH, amt(ChainETH, 1), AssetAmount{}, "0xaddr", false); err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if !strings.Contains(buf.String(), "not in ERC20 registry") {
		t.Fatalf("期望 G1 注册表漏配 ERROR 日志，实际: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "will broadcast as NATIVE ETH") {
		t.Fatalf("期望原生 ETH 错路告警，实际: %q", buf.String())
	}
}

// TestSubmitWithdrawLogsMockFallback 锁定 G3：节点广播全部失败/RPC 不可达时，须输出 ERROR 告警
// （回退模拟广播、实际未上链），而非静默报成功。
func TestSubmitWithdrawLogsMockFallback(t *testing.T) {
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Hour),
		client:              &fakeRPCClient{err: errors.New("node unreachable")},
		signer:              nil,
		erc20:               map[string]ERC20TokenInfo{},
	}
	buf, restore := captureLog(t)
	defer restore()
	if _, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1), AssetAmount{}, "0xaddr", false); err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if !strings.Contains(buf.String(), "MOCK broadcast") {
		t.Fatalf("期望 G3 模拟回退 ERROR 日志，实际: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "NO on-chain tx emitted") {
		t.Fatalf("期望「未上链」标注，实际: %q", buf.String())
	}
}

// TestSubmitWithdrawLogsOfflineSignFail 锁定 G2：离线签名失败须输出 WARN 并降级到节点侧签名广播。
func TestSubmitWithdrawLogsOfflineSignFail(t *testing.T) {
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Hour),
		client:              &fakeRPCClient{hash: "0xnode"},
		signer:              &failSigner{err: errors.New("hsm down")},
		erc20:               map[string]ERC20TokenInfo{},
	}
	buf, restore := captureLog(t)
	defer restore()
	if _, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1), AssetAmount{}, "0xaddr", false); err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if !strings.Contains(buf.String(), "offline sign failed, falling back to node-signed broadcast") {
		t.Fatalf("期望 G2 离线签名失败 WARN，实际: %q", buf.String())
	}
}

// TestSubmitWithdrawLogsOfflineSignEmptyRaw 锁定 G2 补全项：离线签名器返回空 raw（无错误）的
// 静默降级须补 WARN，避免「有意不离线签」不可见。
func TestSubmitWithdrawLogsOfflineSignEmptyRaw(t *testing.T) {
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Hour),
		client:              &fakeRPCClient{hash: "0xnode"},
		signer:              &emptyRawSigner{}, // Sign 返回 ("", nil)
		erc20:               map[string]ERC20TokenInfo{},
	}
	buf, restore := captureLog(t)
	defer restore()
	if _, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1), AssetAmount{}, "0xaddr", false); err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if !strings.Contains(buf.String(), "offline signer returned empty raw, falling back") {
		t.Fatalf("期望 G2 空 raw 降级 WARN，实际: %q", buf.String())
	}
}
