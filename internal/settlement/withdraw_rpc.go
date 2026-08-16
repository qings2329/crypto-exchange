package settlement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	// HotWallet 离线签名边界配置（T-03 热钱包/离线签名）：启用且配了签名器类型时，提现走
	// 「离线签名 → SendRaw 广播原始交易」路径（私钥不出域）；未启用则回退节点侧签名广播
	// （fail-degraded）。生产签名器接 HSM/KMS。
	HotWallet HotWalletConfig `yaml:"hot_wallet"`
}

// HotWalletConfig 是离线签名边界配置。生产填 HSM/KMS 类型；脚手架 "stub" 仅演示边界。
type HotWalletConfig struct {
	// Enabled 是否启用离线签名边界（false → 网关回退节点侧签名广播）。
	Enabled bool `yaml:"enabled"`
	// SignerType 签名器类型：生产 "hsm"/"kms" 走真实 secp256k1 签名；脚手架 "stub" 仅演示边界。
	SignerType string `yaml:"signer_type"`
	// SignerBackend 指定真实签名后端："software"（默认，用 SignerKey 本地私钥）或
	// "external"（用 RegisterExternalSigner 注册的 HSM/KMS 后端；SignerKey 此时作为 keyID）。
	// 生产接入真实安全模块时设 "external" 并事先注册后端，私钥永不离开 HSM/KMS。
	SignerBackend string `yaml:"signer_backend"`
	// SignerKey 是软件签名器本地演示用的私钥（hex，可选 0x 前缀）。生产应由 HSM/KMS 注入，
	// 私钥不出安全域、不落配置；此处仅供离线跑通「真实签名 → SendRaw」链路。
	SignerKey string `yaml:"signer_key"`
	// 以下为 ETH 真实签名的兜底默认值（UnsignedTx 未显式填时采用）。ChainID 为 0 表示
	// 用 pre-EIP-155（legacy v=27/28）；生产主网应填对应 chainID 走 EIP-155。
	EthChainID     uint64 `yaml:"eth_chain_id"`
	EthGasPriceWei uint64 `yaml:"eth_gas_price_wei"`
	EthGasLimit    uint64 `yaml:"eth_gas_limit"`
}

// UnsignedTx 是一笔待签名的提现交易（离线签名边界输入）。仅描述「要签什么」，不含私钥；
// 真实序列化（ETH RLP / BTC UTXO / TRON 合约）与 ECDSA 签名在安全域内完成。
type UnsignedTx struct {
	Chain  Chain
	To     string
	Amount float64
	Asset  string
	// 以下为 ETH 真实签名所需字段（离线签名边界真实密码学用）：nonce/gas/chainID/data。
	// BTC/TRON 暂未实现（各自为独立生产项：UTXO/Nonce 管理、TRON 合约调用），缺省时由
	// 真实签名器按 HotWalletConfig 兜底，仍缺则签名失败 → 网关回退节点侧签名广播（fail-degraded）。
	Nonce       uint64 `yaml:"nonce"`
	GasPriceWei uint64 `yaml:"gas_price_wei"`
	GasLimit    uint64 `yaml:"gas_limit"`
	ChainID     uint64 `yaml:"chain_id"`
	Data        []byte `yaml:"data"`
	// 以下为 BTC 真实签名所需字段：UTXOs 为可选内联未花费输出（为空则签名器经 UTXOSource
	// 按自身地址查询）；ChangeAddress 为找零脚本（hex 或 base58 地址，空→回签名者自身）；
	// FeeRatePerKB 为手续费率（sat/kvB，0→默认 1000）。非 BTC 链忽略这些字段。
	UTXOs         []UTXO `yaml:"utxos"`
	ChangeAddress string `yaml:"change_address"`
	FeeRatePerKB  uint64 `yaml:"fee_rate_per_kb"`
	// 以下为 TRON 真实签名所需字段：ContractAddress 非空时走 TriggerSmartContract（TRC20
	// transfer 合约调用），为空时走 TransferContract（TRX 原生转账）；FeeLimit 为合约调用
	// 的 energy 费用上限（sun，0→不设）。非 TRON 链忽略这些字段。
	ContractAddress string `yaml:"contract_address"`
	FeeLimit        uint64 `yaml:"fee_limit"`
}

// Signer 离线签名边界：在热钱包 / HSM / 安全 enclave 内对交易签名，返回已签名的 raw
// transaction hex（私钥不出域）。生产实现接 HSM/KMS；脚手架提供 stubSigner 演示边界。
type Signer interface {
	Sign(ctx context.Context, tx *UnsignedTx) (rawHex string, err error)
}

// NewSigner 按配置构造签名器：
//   - "stub"：演示签名器（标记化 raw hex，非真实密码学）。
//   - "hsm"/"kms"：真实 secp256k1 签名器（软件等价实现，私钥不出域；生产替换为 HSM/KMS 客户端）。
//     私钥不可用（未配置/非法）时返回 nil，由网关回退节点侧签名广播（fail-degraded）。
//   - 未配置/未知类型：返回 nil，回退节点侧签名广播。
func NewSigner(conf HotWalletConfig) Signer {
	return NewSignerWithSource(conf, SignerSources{})
}

// ETHStateSource 提供 ETH 真实 Nonce / Gas 价查询（节点不可达返回错误，由签名器回退配置/默认值，
// fail-degraded）。生产接真实节点；未注入时签名器按 UnsignedTx 内联字段 / 配置兜底。
type ETHStateSource interface {
	// Nonce 返回账户当前交易计数（"pending"：含内存池未确认）；用于真实 Nonce 管理。
	Nonce(ctx context.Context, chain Chain, account string) (uint64, error)
	// GasPrice 返回节点建议的 gas 价（wei）。
	GasPrice(ctx context.Context, chain Chain) (uint64, error)
}

// SignerSources 是离线签名器可选的「外部状态源」集合：BTC 的 UTXO 源、ETH 的 Nonce/Gas 源、
// TRON 的参考区块源。均为可选；未注入时签名器按 UnsignedTx 内联字段 / 配置默认值兜底（fail-degraded）。
type SignerSources struct {
	UTXOSource UTXOSource     // BTC 真实签名按自身地址向节点查询未花费输出（listunspent）。
	ETHState   ETHStateSource // ETH 真实 Nonce/Gas 管理（eth_getTransactionCount / eth_gasPrice）。
	TRONState  TRONStateSource // TRON 真实签名取参考区块/时间戳（getnowblock），签名哈希源。
}

// TRONStateSource 提供 TRON 真实签名所需的参考区块信息（节点不可达返回错误，由签名器回退
// 节点侧签名广播，fail-degraded）。NowBlock 返回区块号、区块哈希 hex（32 字节）、区块时间戳(ms)。
type TRONStateSource interface {
	NowBlock(ctx context.Context, chain Chain) (blockNum int64, blockID string, timestampMs int64, err error)
}

// NewSignerWithSource 同 NewSigner，但为真实签名器注入可选外部状态源（BTC UTXO 源 / ETH
// Nonce·Gas 源）。stub 签名器忽略 sources；未配/非法密钥返回 nil（回退节点侧签名广播）。
func NewSignerWithSource(conf HotWalletConfig, sources SignerSources) Signer {
	if !conf.Enabled {
		return nil
	}
	switch conf.SignerType {
	case "stub":
		return &stubSigner{}
	case "hsm", "kms":
		if s, err := newRealSignerWithSource(conf, sources); err == nil {
			return s
		}
		return nil // 密钥不可用 → 回退（fail-degraded）
	default:
		return nil
	}
}

// stubSigner 仅用于演示「离线签名边界」：真实场景签名在 HSM/KMS 内完成、私钥不出域，
// 且需按链做 RLP/UTXO/合约序列化 + ECDSA。脚手架不实现真实密码学，返回标记化 raw hex
// 以验证「签名 → SendRaw」链路；生产替换为 HSMWalletSigner。
type stubSigner struct{}

func (s *stubSigner) Sign(ctx context.Context, tx *UnsignedTx) (string, error) {
	// 演示：把待签内容确定性编码为标记化 raw hex（非真实签名）。
	return fmt.Sprintf("0xsigned-chain%s-to%s-amt%v-asset%s", tx.Chain, tx.To, tx.Amount, tx.Asset), nil
}

// DepositWatch 是「链上充值监听」的一条观察项：某链某地址归属某用户某资产。真实 RPC
// 扫描器据此轮询节点，把命中地址的入账解析为 DepositEvent（含 userID 便于账本入账）。
type DepositWatch struct {
	Chain   Chain  `yaml:"chain"`
	Address string `yaml:"address"`
	UserID  int64  `yaml:"user_id"`
	Asset   string `yaml:"asset"`
	// Token 是 TRC20 合约地址（仅 chain=TRON 时用于过滤对应代币）；空则默认 USDT-TRC20 主网合约。
	Token string `yaml:"token"`
}

// ChainRPCClient 抽象单链 RPC 广播能力（T-03 链上 RPC 半边）。生产实现直连节点；
// 单测可用内存假实现注入，便于无节点环境下验证「真实哈希注入」路径。
type ChainRPCClient interface {
	// Broadcast 向链上广播一笔提现（节点侧签名），返回交易哈希。仅用于未启用离线签名
	// 边界的回退路径（fail-degraded）；生产安全的提现应走 Signer + SendRaw。
	Broadcast(ctx context.Context, chain Chain, to string, amount float64) (txHash string, err error)
	// SendRaw 广播一笔已离线签名的原始交易（raw hex），返回交易哈希。离线签名边界主路径：
	// 签名在 Signer（HSM/KMS）内完成，节点仅负责广播。
	SendRaw(ctx context.Context, chain Chain, rawHex string) (txHash string, err error)
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

// Call 是 rpc 的导出包装，供同包内的 UTXO 源等扩展按链发起任意 JSON-RPC 调用并取回
// result（已剔除引号）；节点不可达/报错时返回错误（由调用方回退）。
func (c *JSONRPCClient) Call(ctx context.Context, chain Chain, method string, params []interface{}) (json.RawMessage, error) {
	return c.rpc(ctx, chain, method, params)
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

// SendRaw 广播一笔已离线签名的原始交易，返回交易哈希（离线签名边界主路径）。
//   - ETH：eth_sendRawTransaction（rawHex）。
//   - BTC：sendrawtransaction（rawHex）。
//   - TRON：POST 整笔已签交易 JSON（含 raw_data / txID / signature）到 /wallet/broadcasttransaction，
//     解析响应 txid。rawHex 此处为 signTRON 返回的广播 JSON 字符串。
func (c *JSONRPCClient) SendRaw(ctx context.Context, chain Chain, rawHex string) (string, error) {
	switch chain {
	case ChainETH:
		res, err := c.rpc(ctx, chain, "eth_sendRawTransaction", []interface{}{rawHex})
		if err != nil {
			return "", err
		}
		return string(bytes.Trim(res, `"`)), nil
	case ChainBTC:
		res, err := c.rpc(ctx, chain, "sendrawtransaction", []interface{}{rawHex})
		if err != nil {
			return "", err
		}
		return string(bytes.Trim(res, `"`)), nil
	case ChainTRON:
		body := []byte(rawHex)
		if !json.Valid(body) {
			return "", fmt.Errorf("tron SendRaw 需要 signTRON 返回的广播 JSON 字符串")
		}
		resp, err := c.post(ctx, chain, "/wallet/broadcasttransaction", body)
		if err != nil {
			return "", err
		}
		var out struct {
			Result  bool   `json:"result"`
			Txid    string `json:"txid"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(resp, &out); err != nil {
			return "", fmt.Errorf("tron 广播响应解析失败: %w", err)
		}
		if !out.Result {
			return "", fmt.Errorf("tron 广播被拒: code=%s message=%s", out.Code, out.Message)
		}
		return out.Txid, nil
	default:
		return "", fmt.Errorf("unsupported chain %s", chain)
	}
}

// post 向链端点（TRON 的 REST 风格路径）发起 JSON 请求体 POST，返回响应体字节。
// 与 rpc（JSON-RPC POST）互补；TRON 的 getnowblock / broadcasttransaction 用此路径式接口。
func (c *JSONRPCClient) post(ctx context.Context, chain Chain, path string, body []byte) ([]byte, error) {
	url := c.endpoints[string(chain)]
	if url == "" {
		return nil, fmt.Errorf("no rpc endpoint configured for chain %s", chain)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// NowBlock 查询 TRON 当前参考区块（getnowblock）：返回区块号、区块哈希（32 字节 hex）、
// 区块时间戳(ms)。离线签名器据此构造 ref_block_bytes / ref_block_hash / 过期时间。
func (c *JSONRPCClient) NowBlock(ctx context.Context, chain Chain) (int64, string, int64, error) {
	if chain != ChainTRON {
		return 0, "", 0, fmt.Errorf("NowBlock only supported for TRON (got %s)", chain)
	}
	resp, err := c.post(ctx, chain, "/wallet/getnowblock", []byte("{}"))
	if err != nil {
		return 0, "", 0, err
	}
	var r struct {
		BlockID string `json:"blockID"`
		BlockHeader struct {
			RawData struct {
				Number    int64 `json:"number"`
				Timestamp int64 `json:"timestamp"`
			} `json:"raw_data"`
		} `json:"block_header"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return 0, "", 0, fmt.Errorf("tron 区块解析失败: %w", err)
	}
	if r.BlockID == "" {
		return 0, "", 0, fmt.Errorf("tron 区块 blockID 为空")
	}
	return r.BlockHeader.RawData.Number, r.BlockID, r.BlockHeader.RawData.Timestamp, nil
}

// Nonce 查询 ETH 账户当前交易计数（"pending"：含内存池未确认，避免重放/碰撞）。用于真实
// Nonce 管理：签名器首次签名时取此值，之后本地递增（见 realSigner.resolveNonce）。节点
// 不可达返回错误，由签名器回退配置/默认值（fail-degraded）。
func (c *JSONRPCClient) Nonce(ctx context.Context, chain Chain, account string) (uint64, error) {
	if chain != ChainETH {
		return 0, fmt.Errorf("nonce query only supported for ETH (got %s)", chain)
	}
	res, err := c.rpc(ctx, chain, "eth_getTransactionCount", []interface{}{account, "pending"})
	if err != nil {
		return 0, err
	}
	return parseHexUint(res)
}

// GasPrice 查询节点建议的 gas 价（wei）。用于真实 Gas 管理：未显式配置 gasPrice 时采用节点
// 报价，避免硬编码过时费率。节点不可达返回错误，由签名器回退配置默认值（fail-degraded）。
func (c *JSONRPCClient) GasPrice(ctx context.Context, chain Chain) (uint64, error) {
	if chain != ChainETH {
		return 0, fmt.Errorf("gas price query only supported for ETH (got %s)", chain)
	}
	res, err := c.rpc(ctx, chain, "eth_gasPrice", []interface{}{})
	if err != nil {
		return 0, err
	}
	return parseHexUint(res)
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
//
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
		// TronGrid 风格 REST：取链头区块号与交易所在区块号，差值 +1 为确认数。
		headB, err := c.get(ctx, chain, "/v1/blocks?sort=-number&limit=1")
		if err != nil {
			return 0, err
		}
		head, err := parseTronHeadBlock(headB)
		if err != nil {
			return 0, err
		}
		txB, err := c.get(ctx, chain, "/v1/transactions/"+url.PathEscape(txHash))
		if err != nil {
			return 0, err
		}
		txBlock, found, err := parseTronTxBlock(txB)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, nil // 尚未上链
		}
		return int(head) - int(txBlock) + 1, nil
	default:
		return 0, fmt.Errorf("unsupported chain %s", chain)
	}
}

// parseTronHeadBlock 解析 TronGrid /v1/blocks 响应的链头区块号。
func parseTronHeadBlock(b []byte) (int64, error) {
	var r struct {
		Data []struct {
			Number int64 `json:"number"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	if len(r.Data) == 0 {
		return 0, fmt.Errorf("tron head block not found")
	}
	return r.Data[0].Number, nil
}

// parseTronTxBlock 解析 TronGrid /v1/transactions/{id} 响应的交易所在区块号；未找到返回 found=false。
func parseTronTxBlock(b []byte) (int64, bool, error) {
	var r struct {
		Data []struct {
			BlockNumber int64 `json:"blockNumber"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, false, err
	}
	if len(r.Data) == 0 {
		return 0, false, nil
	}
	return r.Data[0].BlockNumber, true, nil
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

// parseHexUint 同 parseHexInt 但返回无符号（用于 nonce / gasPrice 等非负量）。
func parseHexUint(b []byte) (uint64, error) {
	n, err := parseHexInt(b)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative hex value %d", n)
	}
	return uint64(n), nil
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
// 广播环节支持两种路径：① 配置了离线签名器（Signer）→ 先离线签名再 SendRaw 广播原始
// 交易（私钥不出域，生产接 HSM/KMS）；② 未配置签名器 → 节点侧签名广播（Broadcast，
// fail-degraded）。节点不可达时回退模拟广播，保证无外部节点也能运行。
type RPCWithdrawGateway struct {
	*MockWithdrawGateway
	client ChainRPCClient
	signer Signer // 离线签名器（可选）；nil → 回退节点侧签名广播
}

// SubmitWithdraw 优先走离线签名边界（有 Signer 时：签名→SendRaw 广播并取回真实 TxHash）；
// 无 Signer 或签名/广播失败时回退节点侧签名广播（Broadcast）；RPC 仍不可达则回退模拟。
func (g *RPCWithdrawGateway) SubmitWithdraw(userID int64, asset string, chain Chain, amount, fee float64, address string, willFail bool) (*WithdrawEvent, error) {
	if g.client != nil {
		if g.signer != nil {
			if raw, err := g.signer.Sign(context.Background(), &UnsignedTx{Chain: chain, To: address, Amount: amount, Asset: asset}); err == nil && raw != "" {
				if h, err := g.client.SendRaw(context.Background(), chain, raw); err == nil && h != "" {
					return g.MockWithdrawGateway.SubmitWithdrawWithHash(userID, asset, chain, amount, fee, address, willFail, h)
				}
			}
			// 离线签名/广播失败：回退节点侧签名广播（fail-degraded）。
		}
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
		// 离线签名边界（nil→节点侧签名）：注入了对应链的外部状态源时，该链提现走
		// 「真实链上状态拉取 → 离线签名 → SendRaw」主路径，而非回退节点侧签名广播。
		//   - BTC 端点：注入 UTXO 源（listunspent），避免回退节点侧 sendtoaddress。
		//   - ETH 端点：注入 Nonce/Gas 源（eth_getTransactionCount / eth_gasPrice），避免用过期/默认 Nonce/Gas。
		var sources SignerSources
		if _, ok := conf.Endpoints[string(ChainBTC)]; ok {
			sources.UTXOSource = NewRPCUTXOSource(client)
		}
		if _, ok := conf.Endpoints[string(ChainETH)]; ok {
			sources.ETHState = client // *JSONRPCClient 实现 ETHStateSource
		}
		if _, ok := conf.Endpoints[string(ChainTRON)]; ok {
			sources.TRONState = client // *JSONRPCClient 实现 TRONStateSource（getnowblock）
		}
		return &RPCWithdrawGateway{
			MockWithdrawGateway: mg,
			client:              client,
			signer:              NewSignerWithSource(conf.HotWallet, sources),
		}
	}
	return NewMockWithdrawGateway(req, interval)
}
