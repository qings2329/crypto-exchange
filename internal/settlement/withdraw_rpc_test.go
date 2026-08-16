package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func (f *fakeRPCClient) Broadcast(ctx context.Context, chain Chain, to string, amount float64) (string, error) {
	return f.hash, f.err
}

func (f *fakeRPCClient) SendRaw(ctx context.Context, chain Chain, rawHex string) (string, error) {
	f.sentRaw = true
	f.lastRaw = rawHex
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
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, 100, 0.1, "0xabc", false)
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
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, 100, 0.1, "0xabc", false)
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
	ev, err := g.SubmitWithdraw(1, "USDT", ChainBTC, 0.5, 0.0005, "bc1xyz", false)
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
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, 100, 0.1, "0xabc", false)
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
	ev, err := g.SubmitWithdraw(1, "USDT", ChainBTC, 0.5, 0.0005, "bc1xyz", false)
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

// TestRPCWithdrawGatewayUsesOfflineSigner 验证配置了签名器时，提现走「离线签名→SendRaw
// 广播原始交易」路径（私钥不出域），返回节点给的真实 TxHash；不经过节点侧 Broadcast。
func TestRPCWithdrawGatewayUsesOfflineSigner(t *testing.T) {
	fc := &fakeRPCClient{hash: "0xREALHASH"}
	g := &RPCWithdrawGateway{
		MockWithdrawGateway: NewMockWithdrawGateway(2, time.Second),
		client:              fc,
		signer:              &fakeSigner{},
	}
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, 100, 0.1, "0xabc", false)
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
	ev, err := g.SubmitWithdraw(1, "USDT", ChainETH, 100, 0.1, "0xabc", false)
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
