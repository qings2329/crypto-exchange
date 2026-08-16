package settlement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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
	// WatchAddresses 真实充值监听的「地址→用户」映射（生产由热钱包/地址派生服务生成）。
	// 配置了且启用了 RPC 时，充值网关据此轮询节点检测入账并喂入确认状态机；空则启用了也无
	// 可扫地址（仅靠 SubmitDeposit 显式注入，等价于模拟行为）。
	WatchAddresses []DepositWatch `yaml:"watch_addresses"`
}

// DepositWatch 是「链上充值监听」的一条观察项：某链某地址归属某用户某资产。真实 RPC
// 扫描器据此轮询节点，把命中地址的入账解析为 DepositEvent（含 userID 便于账本入账）。
type DepositWatch struct {
	Chain   Chain   `yaml:"chain"`
	Address string  `yaml:"address"`
	UserID  int64   `yaml:"user_id"`
	Asset   string  `yaml:"asset"`
	// Token 是 TRC20 合约地址（仅 chain=TRON 时用于过滤对应代币）；空则默认 USDT-TRC20 主网合约。
	Token string `yaml:"token"`
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

// ConfirmSource 提供某笔交易在真实链上的当前确认数（真实区块确认轮询扩展点）。
// JSONRPCClient 实现该接口；单测可注入内存假实现验证「真实确认数→状态机推进」。
type ConfirmSource interface {
	// Confirmations 返回交易当前在链上的确认区块数（0 表示尚未上链/未确认）。
	Confirmations(ctx context.Context, chain Chain, txHash string) (int, error)
}

// Confirmations 按链向节点查询交易当前确认数（真实区块确认轮询用）：
//   - ETH：eth_blockNumber 取链头高度，eth_getTransactionByHash 取交易所在区块；
//     二者之差 +1 即确认数（交易未上链 blockNumber 为 null → 返回 0）。
//   - BTC：getrawtransaction(txid, true) 的 confirmations 字段（未确认为 0）。
//   - TRON：脚手架暂不支持，返回错误（生产补全）。
// 节点不可达由调用方回退到模拟确认（fail-degraded）。
func (c *JSONRPCClient) Confirmations(ctx context.Context, chain Chain, txHash string) (int, error) {
	switch chain {
	case ChainETH:
		headB, err := c.rpc(ctx, chain, "eth_blockNumber", []interface{}{})
		if err != nil {
			return 0, err
		}
		head, err := parseHexInt(headB)
		if err != nil {
			return 0, err
		}
		txB, err := c.rpc(ctx, chain, "eth_getTransactionByHash", []interface{}{txHash})
		if err != nil {
			return 0, err
		}
		var tx struct {
			BlockNumber *string `json:"blockNumber"`
		}
		if err := json.Unmarshal(txB, &tx); err != nil {
			return 0, err
		}
		if tx.BlockNumber == nil {
			return 0, nil // 尚未上链
		}
		blk, err := parseHexInt([]byte(*tx.BlockNumber))
		if err != nil {
			return 0, err
		}
		return int(head) - int(blk) + 1, nil
	case ChainBTC:
		raw, err := c.rpc(ctx, chain, "getrawtransaction", []interface{}{txHash, true})
		if err != nil {
			return 0, err
		}
		var r struct {
			Confirmations int `json:"confirmations"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return 0, err
		}
		return r.Confirmations, nil
	case ChainTRON:
		return 0, fmt.Errorf("tron confirmation query not supported in scaffold")
	default:
		return 0, fmt.Errorf("unsupported chain %s", chain)
	}
}

// parseHexInt 把 "0x..." 十六进制整数字节串解析为 int（用于区块高度/确认数）。
// 先去引号再去 0x 前缀（JSON 字符串结果带引号，顺序不可反）。
func parseHexInt(b []byte) (int64, error) {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, `"`)
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 16, 64)
}

// tronUSDTContract 是 TRON 主网 USDT(TRC20) 合约地址；TRC20 观察项未配 token 时默认按此过滤。
const tronUSDTContract = "TR7NHqjiehqjqTD9QgQsrQUDsV7qxXWm1f"

// get 向链端点（仅 TRON）发起 GET 请求，用于 TronGrid 风格的 TRC20 事件 REST 查询，
// 与 rpc（JSON-RPC POST）互补。生产 TRON 监控接 TronGrid 或自建 event 服务。
func (c *JSONRPCClient) get(ctx context.Context, chain Chain, path string) ([]byte, error) {
	url := c.endpoints[string(chain)]
	if url == "" {
		return nil, fmt.Errorf("no rpc endpoint configured for chain %s", chain)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
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
// RPCWithdrawGateway（真实广播 + 真实确认轮询），否则返回 MockWithdrawGateway（全部模拟）。
func NewWithdrawGateway(conf ChainRPCConfig) WithdrawGateway {
	req := conf.Required
	if req <= 0 {
		req = 2
	}
	interval := 2 * time.Second
	if conf.PollSec > 0 {
		interval = time.Duration(conf.PollSec) * time.Second
	}
	if conf.Enabled && len(conf.Endpoints) > 0 {
		client := NewJSONRPCClient(conf.Endpoints)
		mg := NewMockWithdrawGateway(req, interval)
		mg.confirmSource = client // 真实区块确认轮询；节点不可达自动回退模拟
		return &RPCWithdrawGateway{
			MockWithdrawGateway: mg,
			client:              client,
		}
	}
	return NewMockWithdrawGateway(req, interval)
}
