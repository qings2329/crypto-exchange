package settlement

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDepositScanner 是 DepositScanner 的内存假实现，由测试直接推送充值事件。
type fakeDepositScanner struct {
	ch chan DepositEvent
}

func (f *fakeDepositScanner) Scan(ctx context.Context) (<-chan DepositEvent, error) {
	return f.ch, nil
}

// TestNewDepositGatewayDisabledReturnsMock 验证未启用 RPC 时回退到模拟网关，
// SubmitDeposit 行为不变（零回归）。
func TestNewDepositGatewayDisabledReturnsMock(t *testing.T) {
	g := NewDepositGateway(ChainRPCConfig{Enabled: false})
	ev, err := g.SubmitDeposit(1, "USDT", ChainETH, amt(ChainETH, 100), "0xabc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev == nil || ev.TxHash == "" {
		t.Fatalf("expected non-empty simulated tx hash")
	}
	if _, ok := g.(*RPCDepositGateway); ok {
		t.Fatalf("disabled gateway must not be *RPCDepositGateway")
	}
}

// TestRPCDepositGatewayFeedsScannedDeposit 验证配置了扫描器时，节点监听到的充值经
// StartScan 喂入确认状态机，Tick 后达到 Credited（证明真实扫描→状态机链路打通）。
func TestRPCDepositGatewayFeedsScannedDeposit(t *testing.T) {
	sc := &fakeDepositScanner{ch: make(chan DepositEvent, 1)}
	g := &RPCDepositGateway{
		MockChainGateway: NewMockChainGateway(2, time.Second),
		scanner:          sc,
	}
	g.Start()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.StartScan(ctx)

	// 扫描器推送一笔充值入账（模拟节点 eth_getLogs 命中观察地址）。
	// 注意：MockChainGateway.SubmitDeposit 会自行生成 TxHash，故断言按 userID/amount 匹配。
	sc.ch <- DepositEvent{UserID: 7, Asset: "ETH", Chain: ChainETH, Amount: amt(ChainETH, 1.5), Address: "0xWATCH", TxHash: "0xSCAN1"}

	// 等待扫描 goroutine 把它 SubmitDeposit 进 pending。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(g.Pending()) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scanned deposit was not fed into gateway pending")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 推进两个区块使确认数达 Required，置为 Credited。
	g.Tick()
	g.Tick()
	found := false
	for _, ev := range g.Pending() {
		if ev.UserID == 7 && amtEq(ev.Amount, 1.5) && ev.Asset == "ETH" {
			found = true
			if ev.Status != DepositCredited {
				t.Fatalf("expected Credited, got %s", ev.Status)
			}
		}
	}
	if !found {
		t.Fatalf("scanned deposit (user 7, 1.5 ETH) not found in pending after ticks")
	}
}

// TestNewDepositGatewayEnabledUsesRPC 验证启用且配置了 endpoints + watch_addresses 时
// 返回 RPC 充值网关，且满足 DepositGateway 契约（编译期接口检查）。
func TestNewDepositGatewayEnabledUsesRPC(t *testing.T) {
	g := NewDepositGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": "http://127.0.0.1:8545"},
		WatchAddresses: []DepositWatch{
			{Chain: ChainETH, Address: "0xWATCH", UserID: 1, Asset: "ETH"},
		},
	})
	var _ DepositGateway = g // 编译期契约检查
	if _, ok := g.(*RPCDepositGateway); !ok {
		t.Fatalf("enabled gateway should be *RPCDepositGateway, got %T", g)
	}
}

// TestJSONRPCDepositScannerETH 用 httptest 模拟节点 eth_getLogs 响应，验证真实扫描器把
// 命中观察地址的 Transfer 日志解析为 DepositEvent（value 0x0de0b6b3a7640000 = 1e18 wei = 1.0）。
func TestJSONRPCDepositScannerETH(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[` +
			`{"transactionHash":"0xabc","data":"0x0de0b6b3a7640000","address":"0xWATCH"}` +
			`]}`))
	}))
	defer srv.Close()

	sc := NewJSONRPCDepositScanner(
		NewJSONRPCClient(map[string]string{"ETH": srv.URL}),
		[]DepositWatch{{Chain: ChainETH, Address: "0xWATCH", UserID: 9, Asset: "ETH"}},
		20*time.Millisecond,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := sc.Scan(ctx)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.UserID != 9 || ev.Asset != "ETH" || ev.Chain != ChainETH {
			t.Fatalf("unexpected event identity: %+v", ev)
		}
		if !amtEq(ev.Amount, 1.0) {
			t.Fatalf("expected amount 1.0 (1e18 wei), got %v", ev.Amount)
		}
		if !strings.EqualFold(ev.TxHash, "0xabc") {
			t.Fatalf("unexpected tx hash: %q", ev.TxHash)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no deposit event scanned from node")
	}
}

// fakeDepositConfirmSource 是 ConfirmSource 的内存假实现（充值确认轮询测试用）。
type fakeDepositConfirmSource struct {
	conf int
	err  error
}

func (f *fakeDepositConfirmSource) Confirmations(ctx context.Context, chain Chain, txHash string) (int, error) {
	return f.conf, f.err
}

// TestRPCDepositGatewayUsesRealConfirmations 验证配置了确认源时，充值确认数按节点返回的
// 真实确认数推进（而非模拟 +1），达标即置 Credited（真实区块确认轮询路径）。
func TestRPCDepositGatewayUsesRealConfirmations(t *testing.T) {
	g := &RPCDepositGateway{
		MockChainGateway: NewMockChainGateway(2, time.Second),
	}
	g.MockChainGateway.confirmSource = &fakeDepositConfirmSource{conf: 3} // 节点返回 3 个确认
	ev, err := g.SubmitDeposit(1, "ETH", ChainETH, amt(ChainETH, 1.5), "0xWATCH")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g.Tick() // 真实确认数 3 >= 2 -> Credited
	found := false
	for _, e := range g.Pending() {
		if e.TxHash == ev.TxHash {
			found = true
			if e.Status != DepositCredited {
				t.Fatalf("expected Credited, got %s", e.Status)
			}
			if e.Confirmations != 3 {
				t.Fatalf("expected real confirmations 3, got %d", e.Confirmations)
			}
		}
	}
	if !found {
		t.Fatalf("deposit not found after tick")
	}
}

// TestRPCDepositGatewayFallsBackOnConfirmError 验证确认源不可达时自动回退模拟 +1（fail-degraded）。
func TestRPCDepositGatewayFallsBackOnConfirmError(t *testing.T) {
	g := &RPCDepositGateway{
		MockChainGateway: NewMockChainGateway(2, time.Second),
	}
	g.MockChainGateway.confirmSource = &fakeDepositConfirmSource{err: errors.New("node unreachable")}
	ev, err := g.SubmitDeposit(1, "BTC", ChainBTC, amt(ChainBTC, 0.5), "bc1xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g.Tick() // 回退模拟 0+1=1
	g.Tick() // 回退模拟 1+1=2 -> Credited
	found := false
	for _, e := range g.Pending() {
		if e.TxHash == ev.TxHash {
			found = true
			if e.Status != DepositCredited {
				t.Fatalf("expected Credited, got %s", e.Status)
			}
			if e.Confirmations != 2 {
				t.Fatalf("expected simulated confirmations 2, got %d", e.Confirmations)
			}
		}
	}
	if !found {
		t.Fatalf("deposit not found after ticks")
	}
}

// TestJSONRPCDepositScannerTRON 用 httptest 模拟 TronGrid TRC20 事件响应，验证真实扫描器
// 过滤出 to==观察地址的入账（value 1000000/1e6 = 1.0），并忽略 to 不匹配的转出。
func TestJSONRPCDepositScannerTRON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` +
			`{"transaction_id":"0xtron1","token_info":{"decimals":6},"value":"1000000","to":"TRwatch"}` +
			`,{"transaction_id":"0xtron2","token_info":{"decimals":6},"value":"500000","to":"TRother"}` +
			`]}`))
	}))
	defer srv.Close()

	sc := NewJSONRPCDepositScanner(
		NewJSONRPCClient(map[string]string{"TRON": srv.URL}),
		[]DepositWatch{{Chain: ChainTRON, Address: "TRwatch", UserID: 3, Asset: "USDT"}},
		20*time.Millisecond,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := sc.Scan(ctx)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	for ev := range ch {
		if ev.TxHash == "0xtron2" {
			t.Fatalf("non-matching 'to' transfer must be filtered out: %+v", ev)
		}
		if ev.TxHash == "0xtron1" {
			if ev.UserID != 3 || ev.Asset != "USDT" || ev.Chain != ChainTRON {
				t.Fatalf("unexpected event identity: %+v", ev)
			}
			if !amtEq(ev.Amount, 1.0) {
				t.Fatalf("expected amount 1.0 (1000000/1e6), got %v", ev.Amount)
			}
			return // 命中即成功
		}
	}
	t.Fatalf("no matching TRC20 deposit event scanned")
}
