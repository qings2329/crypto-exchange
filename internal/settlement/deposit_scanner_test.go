package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// depositNodeMock 充当「假链上节点」，覆盖扫描器依赖的三种端点：
//   - POST eth_getLogs：返回一条 ERC20 Transfer 日志（data=1e18 wei=1.0）。
//   - POST listsinceblock：返回一条 BTC receive 入账（0.5）。
//   - GET /v1/accounts/<addr>/transactions/trc20：返回一条 TRC20 入账（to 回显 path 中的地址，
//     使过滤器命中；可用 tronTo 覆写为其它地址以验证「非本地址忽略」）。
//
// 通过 opts 可注入异常（badJSON/自定义 tronTo/tronValue）以覆盖负路径。
type depositNodeOpts struct {
	badJSON   bool
	tronTo    string // 非空则作为返回 data 中的 to（与 watch 地址不匹配 → 被过滤）
	tronValue string // TRC20 value（最小单位），默认 "1000000"（6 位小数=1.0）
}

func depositNodeMock(t *testing.T, opts depositNodeOpts) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if opts.badJSON {
			_, _ = w.Write([]byte("not-json"))
			return
		}
		if r.Method == http.MethodGet {
			// TRON TRC20 REST
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			addr := ""
			if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "accounts" {
				addr = parts[2]
			}
			to := opts.tronTo
			if to == "" {
				to = addr
			}
			val := opts.tronValue
			if val == "" {
				val = "1000000"
			}
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"data":[{"transaction_id":"trx1","token_info":{"decimals":6},"value":%q,"to":%q}]}`, val, to)))
			return
		}
		// ETH / BTC JSON-RPC
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "eth_getLogs":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"transactionHash":"0xethtx1","data":"0xDE0B6B3A7640000"}]}`))
		case "listsinceblock":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"transactions":[{"txid":"btctx1","address":"btcwatch","amount":0.5,"category":"receive","confirmations":3}]}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
		}
	}))
}

func newScanClient(t *testing.T, url string) *JSONRPCClient {
	t.Helper()
	return NewJSONRPCClient(map[string]string{
		string(ChainETH):  url,
		string(ChainBTC):  url,
		string(ChainTRON): url,
	})
}

// —— 纯函数 ——

func TestWeiToAmount(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0xDE0B6B3A7640000", 1.0}, // 1 ETH in wei
		{"0x0", 0},
		{"", 0},
		{"0x", 0},
		{"0x10", 1.6e-17},
		{"not-hex", 0},
		{`"0xDE0B6B3A7640000"`, 1.0}, // 带引号
	}
	for _, c := range cases {
		if got := weiToAmount(c.in); got.HumanFloat() != c.want {
			t.Fatalf("weiToAmount(%q)=%v want %v", c.in, got.HumanFloat(), c.want)
		}
	}
}

func TestTronAmountToFloat(t *testing.T) {
	if got := tronAmountToFloat("1000000", 6); got.HumanFloat() != 1.0 {
		t.Fatalf("tronAmountToFloat(1000000,6)=%v want 1.0", got.HumanFloat())
	}
	if got := tronAmountToFloat("1", 0); got.HumanFloat() != 1.0 {
		t.Fatalf("tronAmountToFloat(1,0)=%v want 1.0", got.HumanFloat())
	}
	if got := tronAmountToFloat("not-int", 6); got.HumanFloat() != 0 {
		t.Fatalf("tronAmountToFloat(非法)=%v want 0", got.HumanFloat())
	}
}

// —— 各链扫描 ——

func TestScanETH(t *testing.T) {
	node := depositNodeMock(t, depositNodeOpts{})
	defer node.Close()
	s := NewJSONRPCDepositScanner(newScanClient(t, node.URL), nil, 0)
	evs, err := s.scanETH(context.Background(), DepositWatch{UserID: 7, Asset: "ETH", Chain: ChainETH, Address: "0xwatch"})
	if err != nil {
		t.Fatalf("scanETH: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("应解析出 1 条 ETH 入账，got %d", len(evs))
	}
	e := evs[0]
	if !amtEq(e.Amount, 1.0) || e.TxHash != "0xethtx1" || e.UserID != 7 || e.Chain != ChainETH || e.Asset != "ETH" {
		t.Fatalf("ETH 入账字段不符: %+v", e)
	}

	// 节点返回非法 JSON → 解析错误。
	bad := depositNodeMock(t, depositNodeOpts{badJSON: true})
	defer bad.Close()
	s2 := NewJSONRPCDepositScanner(newScanClient(t, bad.URL), nil, 0)
	if _, err := s2.scanETH(context.Background(), DepositWatch{Chain: ChainETH}); err == nil {
		t.Fatalf("eth_getLogs 返回非法 JSON 应报错")
	}
}

func TestScanBTC(t *testing.T) {
	node := depositNodeMock(t, depositNodeOpts{})
	defer node.Close()
	s := NewJSONRPCDepositScanner(newScanClient(t, node.URL), nil, 0)
	// 默认 mock 返回的 receive 地址是 "btcwatch"，用相同地址观察可命中。
	evs, err := s.scanBTC(context.Background(), DepositWatch{UserID: 3, Asset: "BTC", Chain: ChainBTC, Address: "btcwatch"})
	if err != nil {
		t.Fatalf("scanBTC: %v", err)
	}
	if len(evs) != 1 || !amtEq(evs[0].Amount, 0.5) || evs[0].TxHash != "btctx1" {
		t.Fatalf("BTC 入账不符: %+v", evs)
	}

	// 观察地址与入账地址不一致 → 过滤（不产生错误，仅 0 条）。
	evs2, err := s.scanBTC(context.Background(), DepositWatch{UserID: 3, Chain: ChainBTC, Address: "other"})
	if err != nil {
		t.Fatalf("scanBTC(地址不符): %v", err)
	}
	if len(evs2) != 0 {
		t.Fatalf("地址不符应被过滤，got %d", len(evs2))
	}
}

func TestScanTRON(t *testing.T) {
	// to 回显为观察地址 → 命中。
	node := depositNodeMock(t, depositNodeOpts{})
	defer node.Close()
	s := NewJSONRPCDepositScanner(newScanClient(t, node.URL), nil, 0)
	addr := "T9y7Q9x2m3n4b5v6c7x8z9a1s2d3f4g5h6j7k8l"
	evs, err := s.scanTRON(context.Background(), DepositWatch{UserID: 9, Asset: "USDT", Chain: ChainTRON, Address: addr})
	if err != nil {
		t.Fatalf("scanTRON: %v", err)
	}
	if len(evs) != 1 || !amtEq(evs[0].Amount, 1.0) || evs[0].TxHash != "trx1" || evs[0].Chain != ChainTRON {
		t.Fatalf("TRON 入账不符: %+v", evs)
	}

	// to 与观察地址不一致 → 过滤。
	node2 := depositNodeMock(t, depositNodeOpts{tronTo: "Tdifferentaddressxxxxxxxxxxxxxxxxxxxxxxx"})
	defer node2.Close()
	s2 := NewJSONRPCDepositScanner(newScanClient(t, node2.URL), nil, 0)
	evs2, err := s2.scanTRON(context.Background(), DepositWatch{UserID: 9, Chain: ChainTRON, Address: addr})
	if err != nil {
		t.Fatalf("scanTRON(地址不符): %v", err)
	}
	if len(evs2) != 0 {
		t.Fatalf("TRON 非本地址应被过滤，got %d", len(evs2))
	}
}

func TestScanChainUnsupported(t *testing.T) {
	node := depositNodeMock(t, depositNodeOpts{})
	defer node.Close()
	s := NewJSONRPCDepositScanner(newScanClient(t, node.URL), nil, 0)
	if _, err := s.scanChain(context.Background(), DepositWatch{Chain: "DOGE"}); err == nil {
		t.Fatalf("不支持的链应报错")
	}
}

// —— scanOnce 去重与节点不可达 ——

func TestScanOnceDedup(t *testing.T) {
	node := depositNodeMock(t, depositNodeOpts{})
	defer node.Close()
	s := NewJSONRPCDepositScanner(newScanClient(t, node.URL),
		[]DepositWatch{{UserID: 1, Asset: "ETH", Chain: ChainETH, Address: "0xw"}}, 0)
	out := make(chan DepositEvent, 64)
	seen := make(map[string]bool)

	s.scanOnce(context.Background(), out, seen)
	s.scanOnce(context.Background(), out, seen) // 重复扫描同一笔 → 应去重

	close(out)
	n := 0
	for range out {
		n++
	}
	if n != 1 {
		t.Fatalf("同一笔交易去重后应只推送 1 次，got %d", n)
	}
}

func TestScanOnceNodeUnreachable(t *testing.T) {
	// 指向不可达端点 → scanETH 报错，scanOnce 吞掉错误、不产生事件、不 panic。
	s := NewJSONRPCDepositScanner(newScanClient(t, "http://127.0.0.1:1"),
		[]DepositWatch{{UserID: 1, Chain: ChainETH, Address: "0xw"}}, 0)
	out := make(chan DepositEvent, 64)
	seen := make(map[string]bool)
	s.scanOnce(context.Background(), out, seen)
	close(out)
	if len(out) != 0 {
		t.Fatalf("节点不可达不应产生事件，got %d", len(out))
	}
}

// —— Scan 生命周期 ——

func TestScannerScanIntegration(t *testing.T) {
	node := depositNodeMock(t, depositNodeOpts{})
	defer node.Close()
	s := NewJSONRPCDepositScanner(newScanClient(t, node.URL),
		[]DepositWatch{{UserID: 1, Asset: "ETH", Chain: ChainETH, Address: "0xw"}}, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.TxHash != "0xethtx1" {
			t.Fatalf("未收到预期 ETH 入账: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("超时未收到扫描事件")
	}
	cancel()
	// ctx 取消后 channel 应关闭。
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("ctx 取消后 channel 不应再产生事件")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ctx 取消后 channel 未关闭")
	}
}

func TestNewJSONRPCDepositScannerPollDefault(t *testing.T) {
	s := NewJSONRPCDepositScanner(nil, nil, 0)
	if s.poll != 2*time.Second {
		t.Fatalf("poll<=0 应默认 2s，got %v", s.poll)
	}
}

func TestScannerScanNilClient(t *testing.T) {
	_, err := NewJSONRPCDepositScanner(nil, nil, 0).Scan(context.Background())
	if err == nil {
		t.Fatalf("client 为 nil 应报错")
	}
}

// —— StartScan 把扫描到的入账喂入网关确认状态机 ——

func TestStartScanFeedsGateway(t *testing.T) {
	node := depositNodeMock(t, depositNodeOpts{})
	defer node.Close()
	scanner := NewJSONRPCDepositScanner(newScanClient(t, node.URL),
		[]DepositWatch{{UserID: 5, Asset: "ETH", Chain: ChainETH, Address: "0xw"}}, 10*time.Millisecond)
	g := &RPCDepositGateway{
		MockChainGateway: NewMockChainGateway(2, time.Hour),
		scanner:          scanner,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.StartScan(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(g.Pending()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pending := g.Pending()
	if len(pending) != 1 {
		t.Fatalf("StartScan 应把扫描到的入账喂入网关，pending=%d", len(pending))
	}
	// txHash 透传改进：扫描到的真实链上 txHash 应保留，而非网关自生成哈希。
	if pending[0].TxHash != "0xethtx1" {
		t.Fatalf("StartScan 应透传扫描事件的真实 txHash，got %q", pending[0].TxHash)
	}
	if pending[0].UserID != 5 || !amtEq(pending[0].Amount, 1.0) || pending[0].Chain != ChainETH || pending[0].Address != "0xw" {
		t.Fatalf("喂入的入账不符: %+v", pending[0])
	}
}

// TestSubmitDepositWithHash 验证真实链上 txHash 透传：传入非空 txHash 时入账保留该哈希
// （用于链上幂等/对账），为空时回退本地生成；pending 以 txHash 为键故重复提交幂等。
func TestSubmitDepositWithHash(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour)

	ev, err := g.SubmitDepositWithHash(5, "ETH", ChainETH, amt(ChainETH, 1.0), "0xw", "0xrealonchainhash")
	if err != nil {
		t.Fatalf("SubmitDepositWithHash: %v", err)
	}
	if ev.TxHash != "0xrealonchainhash" {
		t.Fatalf("应保留传入的真实 txHash，got %q", ev.TxHash)
	}

	// 空 txHash 回退本地生成（非空且区别于传入值）。
	ev2, err := g.SubmitDepositWithHash(5, "ETH", ChainETH, amt(ChainETH, 1.0), "0xw", "")
	if err != nil || ev2.TxHash == "" || ev2.TxHash == "0xrealonchainhash" {
		t.Fatalf("空 txHash 应回退本地生成，got err=%v hash=%q", err, ev2.TxHash)
	}

	// 重复提交同一 txHash → 幂等（pending 以 txHash 为键，不重复入账）。
	if _, err := g.SubmitDepositWithHash(5, "ETH", ChainETH, amt(ChainETH, 1.0), "0xw", "0xrealonchainhash"); err != nil {
		t.Fatalf("重复提交应成功: %v", err)
	}
	if len(g.Pending()) != 2 { // 仅两条不同 txHash（真实 + 回退生成）
		t.Fatalf("重复 txHash 应幂等，pending=%d", len(g.Pending()))
	}

	// 非法参数仍拒绝。
	if _, err := g.SubmitDepositWithHash(0, "ETH", ChainETH, amt(ChainETH, 1.0), "0xw", "x"); err == nil {
		t.Fatalf("zero user 应被拒绝")
	}
}
