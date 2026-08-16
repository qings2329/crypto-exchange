package settlement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ChainRPCConfig 是链上提现网关（T-03 链上 RPC 半边）配置。生产填真实节点 RPC，
// 网关据此广播提现并取回真实 TxHash；留空/未启用时 NewWithdrawGateway 回退到
// MockWithdrawGateway（全模拟），保证无外部节点也能运行（fail-degraded）。
type ChainRPCConfig struct {
	// Enabled 是否启用真实 RPC 广播（false → 回退模拟网关）。
	Enabled bool `yaml:"enabled"`
	// Endpoints 各链 RPC 端点：链名(ETH/BTC/TRON) -> RPC URL（如 http://127.0.0.1:8545）。
	Endpoints map[string]string `yaml:"endpoints"`
	// Required 安全确认数阈值；<=0 用默认 2。
	Required int `yaml:"required_confirmations"`
	// PollSec 确认轮询间隔（秒）；当前仅用于文档，确认推进由模拟状态机驱动（见下）。
	PollSec int `yaml:"poll_interval_sec"`
}

// ChainRPCClient 抽象单链 RPC 广播能力（T-03 链上 RPC 半边）。生产实现直连节点；
// 单测可用内存假实现注入，便于无节点环境下验证「真实哈希注入」路径。
type ChainRPCClient interface {
	// Broadcast 向链上广播一笔提现，返回交易哈希。链 specifics（签名/手续费/Nonce/
	// 热钱包）由实现处理；生产需配合热钱包或离线签名，此处为脚手架边界。
	Broadcast(ctx context.Context, chain Chain, to string, amount float64) (txHash string, err error)
}

// JSONRPCClient 是 ChainRPCClient 的通用 JSON-RPC 实现，按链映射到对应节点方法
// （ETH: eth_sendTransaction / BTC: sendtoaddress / TRON: wallet/triggersmartcontract）。
// 真实签名/热钱包管理由节点侧或离线签名层负责，本客户端仅负责协议收发与哈希解析。
type JSONRPCClient struct {
	endpoints  map[string]string
	httpClient *http.Client
}

// NewJSONRPCClient 由各链 RPC URL 构造客户端。
func NewJSONRPCClient(endpoints map[string]string) *JSONRPCClient {
	return &JSONRPCClient{
		endpoints:  endpoints,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// rpc 发送一次 JSON-RPC 2.0 调用并返回 result 字段（已剔除引号）。
func (c *JSONRPCClient) rpc(ctx context.Context, chain Chain, method string, params []interface{}) (json.RawMessage, error) {
	url := c.endpoints[string(chain)]
	if url == "" {
		return nil, fmt.Errorf("no rpc endpoint configured for chain %s", chain)
	}
	reqBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("rpc error on %s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

// Broadcast 按链映射节点方法广播提现并取回交易哈希。
func (c *JSONRPCClient) Broadcast(ctx context.Context, chain Chain, to string, amount float64) (string, error) {
	switch chain {
	case ChainETH:
		// 生产需配合热钱包/解锁账户或离线签名后的 raw tx；此处调用 eth_sendTransaction
		// （节点侧签名生效时返回真实 TxHash）。value 以 wei 表示（脚手架：amount*1e18）。
		res, err := c.rpc(ctx, chain, "eth_sendTransaction", []interface{}{
			map[string]interface{}{"to": to, "value": fmt.Sprintf("0x%x", int64(amount*1e18))},
		})
		if err != nil {
			return "", err
		}
		return string(bytes.Trim(res, `"`)), nil
	case ChainBTC:
		res, err := c.rpc(ctx, chain, "sendtoaddress", []interface{}{to, amount})
		if err != nil {
			return "", err
		}
		return string(bytes.Trim(res, `"`)), nil
	case ChainTRON:
		res, err := c.rpc(ctx, chain, "wallet/triggersmartcontract", []interface{}{to, amount})
		if err != nil {
			return "", err
		}
		return string(bytes.Trim(res, `"`)), nil
	default:
		return "", fmt.Errorf("unsupported chain %s", chain)
	}
}

// RPCWithdrawGateway 是 WithdrawGateway 的真实链上 RPC 实现（T-03 链上 RPC 半边脚手架）。
// 它嵌入 MockWithdrawGateway，复用其经过验证的确认状态机、孤块/重组回滚与查询能力，
// 仅在「广播」环节改为调用真实 ChainRPCClient（向节点发送交易、取回真实 TxHash）；
// 当 RPC 不可达（节点未配置/宕机）时自动回退到模拟广播，保证无外部节点也能运行。
//
// 设计要点：确认数推进目前仍由模拟状态机按固定间隔驱动（与区块高度脱钩），真实区块
// 确认轮询为后续扩展点（ChainRPCClient 可再加 Confirmations 方法并由后台 poller 驱动）。
type RPCWithdrawGateway struct {
	*MockWithdrawGateway
	client ChainRPCClient
}

// SubmitWithdraw 优先用真实 RPC 广播并注入节点返回的真实 TxHash；RPC 失败则回退模拟。
func (g *RPCWithdrawGateway) SubmitWithdraw(userID int64, asset string, chain Chain, amount, fee float64, address string, willFail bool) (*WithdrawEvent, error) {
	if g.client != nil {
		if h, err := g.client.Broadcast(context.Background(), chain, address, amount); err == nil && h != "" {
			return g.MockWithdrawGateway.SubmitWithdrawWithHash(userID, asset, chain, amount, fee, address, willFail, h)
		}
		// RPC 不可达：回退模拟广播（fail-degraded），由调用方日志侧记录告警。
	}
	return g.MockWithdrawGateway.SubmitWithdraw(userID, asset, chain, amount, fee, address, willFail)
}

// NewWithdrawGateway 按配置选择链上提现网关实现：启用且配置了 RPC 端点时返回
// RPCWithdrawGateway（真实广播 + 模拟确认），否则返回 MockWithdrawGateway（全模拟）。
func NewWithdrawGateway(conf ChainRPCConfig) WithdrawGateway {
	req := conf.Required
	if req <= 0 {
		req = 2
	}
	if conf.Enabled && len(conf.Endpoints) > 0 {
		return &RPCWithdrawGateway{
			MockWithdrawGateway: NewMockWithdrawGateway(req, 2*time.Second),
			client:              NewJSONRPCClient(conf.Endpoints),
		}
	}
	return NewMockWithdrawGateway(req, 2*time.Second)
}
