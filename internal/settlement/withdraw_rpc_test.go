package settlement

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeRPCClient 是 ChainRPCClient 的内存假实现，用于无节点环境下验证「真实哈希注入」路径。
type fakeRPCClient struct {
	hash string
	err  error
}

func (f *fakeRPCClient) Broadcast(ctx context.Context, chain Chain, to string, amount float64) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.hash, nil
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
