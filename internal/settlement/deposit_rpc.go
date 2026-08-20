package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// DepositScanner 抽象链上充值监听源（T-03 链上 RPC 半边·充值回调脚手架）。生产实现直连
// 节点轮询/订阅入账事件；单测可注入内存假实现验证「扫描→确认状态机」链路。
type DepositScanner interface {
	// Scan 持续从链上监听充值事件，经返回的 channel 推送；ctx 取消即停止。重复交易由
	// 网关按 TxHash 去重，实现可重复推送。
	Scan(ctx context.Context) (<-chan DepositEvent, error)
}

// JSONRPCDepositScanner 是 DepositScanner 的通用 JSON-RPC 实现：按 `watch` 列表轮询各链
// 节点，把命中观察地址的入账解析为 DepositEvent 喂给网关确认状态机。复用 withdraw_rpc.go
// 的 JSONRPCClient 收发协议；真实节点解码在 scanChain 内按链实现：ETH 原生转账走区块扫描
// （eth_getBlockByNumber 全量交易，按 to==观察地址 捕获），ERC20/TRC20 走合约 Transfer 事件，
// BTC/LTC/DOGE 走 listsinceblock。原生 ETH 不复用 eth_getLogs（其无日志，原实现永不入账）。
type JSONRPCDepositScanner struct {
	client  *JSONRPCClient
	watches []DepositWatch
	poll    time.Duration
	// lastBlock 记录每个观察地址（"chain|address"）已扫描到的最高区块高度，避免每次全量回溯、
	// 并防止进程重启后重复扫描历史区块造成双付。原生 ETH 充值走区块扫描（M2），必须有水位线。
	lastBlock map[string]int64
	// blockStarted 标记某观察地址是否已做过首次扫描（首次仅把水位线对齐到当前链头，
	// 不回填历史充值，避免进程重启后重复入账）。
	blockStarted map[string]bool
}

// NewJSONRPCDepositScanner 由各链 RPC 客户端 + 观察地址列表 + 轮询间隔构造扫描器。
func NewJSONRPCDepositScanner(client *JSONRPCClient, watches []DepositWatch, poll time.Duration) *JSONRPCDepositScanner {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	return &JSONRPCDepositScanner{
		client:       client,
		watches:      watches,
		poll:         poll,
		lastBlock:    make(map[string]int64),
		blockStarted: make(map[string]bool),
	}
}

// Scan 启动轮询：立即扫一次，随后按 poll 间隔重复；命中观察地址的入账经 out 推送，按
// TxHash 去重避免重复入账。节点不可达/解码失败仅记录并跳过该次扫描（fail-degraded）。
func (s *JSONRPCDepositScanner) Scan(ctx context.Context) (<-chan DepositEvent, error) {
	if s.client == nil {
		return nil, fmt.Errorf("deposit scanner requires a chain rpc client")
	}
	out := make(chan DepositEvent, 64)
	seen := make(map[string]bool)
	go func() {
		defer close(out)
		s.scanOnce(ctx, out, seen)
		ticker := time.NewTicker(s.poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.scanOnce(ctx, out, seen)
			}
		}
	}()
	return out, nil
}

// scanOnce 对每条观察项调用对应链的扫描，把结果去重后推入 out。
func (s *JSONRPCDepositScanner) scanOnce(ctx context.Context, out chan<- DepositEvent, seen map[string]bool) {
	for _, w := range s.watches {
		evs, err := s.scanChain(ctx, w)
		if err != nil {
			continue // 节点不可达/链不支持：跳过，不阻断其他链
		}
		for _, ev := range evs {
			if seen[ev.TxHash] {
				continue
			}
			seen[ev.TxHash] = true
			select {
			case out <- ev:
			default: // 订阅者阻塞则丢弃，避免阻塞扫描循环
			}
		}
	}
}

// scanChain 按链调用节点方法解析入账（脚手架：ETH 完整解码，BTC 经 listsinceblock，
// TRON 返回空——TRC20 合约事件过滤为生产补全项）。
func (s *JSONRPCDepositScanner) scanChain(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	switch w.Chain {
	case ChainETH:
		return s.scanETH(ctx, w)
	case ChainBTC, ChainLTC, ChainDOGE:
		return s.scanUTXO(ctx, w)
	case ChainTRON:
		return s.scanTRON(ctx, w)
	case ChainSOL:
		return s.scanSOL(ctx, w)
	default:
		return nil, fmt.Errorf("unsupported chain %s", w.Chain)
	}
}

// scanETH 解析 ETH 链充值。w.Token 非空时按 ERC20（合约 Transfer 日志）处理；w.Token 为空时
// 按原生 ETH 处理：原生 ETH 的 value 转账不产生任何日志，故改用区块扫描（eth_getBlockByNumber
// 全量交易）按 to==观察地址 捕获充值。原实现以观察地址为合约地址过滤 eth_getLogs，会因原生转账
// 无日志而永不入账、且误收该地址发出的合约事件（M2/F5），此处修正。
func (s *JSONRPCDepositScanner) scanETH(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	if w.Token != "" {
		return s.scanERC20(ctx, w)
	}
	return s.scanNativeETH(ctx, w)
}

// scanNativeETH 扫描原生 ETH 充值：原生转账不产出日志，只能按区块遍历找出 to==观察地址 且
// value>0 的交易。水位线 lastBlock 保证每次只扫新区块、不回填历史（防进程重启/重复扫描造成双付）。
// decimals 固定 18（#6，无 float 精度损失）。单区块拉取失败时已处理区块的结果照常返回，水位线
// 推进到已处理高度，失败区块下次重试（fail-degraded，不丢弃已确认的充值）。
func (s *JSONRPCDepositScanner) scanNativeETH(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	latest, err := s.ethBlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	key := string(ChainETH) + "|" + w.Address
	if !s.blockStarted[key] {
		// 首次扫描：仅把水位线对齐到当前链头，不回填历史，避免重启后重复入账。
		s.blockStarted[key] = true
		s.lastBlock[key] = latest
		return nil, nil
	}
	from := s.lastBlock[key] + 1
	if from > latest {
		return nil, nil
	}
	// 限制单次扫描区块数，避免极端落后时大量 RPC 调用（脚手架保护）。
	const maxBlocksPerScan = 500
	if latest-from+1 > maxBlocksPerScan {
		if from = latest - maxBlocksPerScan + 1; from < s.lastBlock[key]+1 {
			from = s.lastBlock[key] + 1
		}
	}
	out := make([]DepositEvent, 0)
	processed := from - 1
	for blk := from; blk <= latest; blk++ {
		txs, err := s.ethBlockTxs(ctx, blk)
		if err != nil {
			// 该区块拉取失败：已处理区块的充值照常返回，水位线推进到已处理高度，下次重试失败区块。
			break
		}
		processed = blk
		for _, tx := range txs {
			if tx.To == "" || !strings.EqualFold(tx.To, w.Address) {
				continue // 仅捕获转入观察地址的充值，忽略该地址发出的交易
			}
			amount := weiToAmount(tx.Value)
			if amount.Sign() <= 0 {
				continue
			}
			out = append(out, DepositEvent{
				TxHash:  strings.Trim(tx.Hash, `"`),
				UserID:  w.UserID,
				Asset:   w.Asset,
				Amount:  amount,
				Chain:   ChainETH,
				Address: w.Address,
			})
		}
	}
	s.lastBlock[key] = processed
	return out, nil
}

// ethBlockNumber 取当前 ETH 链头高度（decimal hex）。
func (s *JSONRPCDepositScanner) ethBlockNumber(ctx context.Context) (int64, error) {
	res, err := s.client.rpc(ctx, ChainETH, "eth_blockNumber", []interface{}{})
	if err != nil {
		return 0, err
	}
	h := strings.Trim(strings.TrimSpace(string(res)), `"`)
	n, err := strconv.ParseInt(strings.TrimPrefix(h, "0x"), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid eth_blockNumber result %q: %w", h, err)
	}
	return n, nil
}

// ethTx 是 eth_getBlockByNumber 全量交易模式下的单笔交易结构（仅取充值解析所需字段）。
type ethTx struct {
	Hash  string `json:"hash"`
	To    string `json:"to"`
	Value string `json:"value"`
}

// ethBlockTxs 取指定高度区块的全部交易（full txs 模式）。
func (s *JSONRPCDepositScanner) ethBlockTxs(ctx context.Context, block int64) ([]ethTx, error) {
	res, err := s.client.rpc(ctx, ChainETH, "eth_getBlockByNumber", []interface{}{
		fmt.Sprintf("0x%x", block), true,
	})
	if err != nil {
		return nil, err
	}
	var blk struct {
		Transactions []ethTx `json:"transactions"`
	}
	if err := json.Unmarshal(res, &blk); err != nil {
		return nil, err
	}
	return blk.Transactions, nil
}

// erc20TransferTopic 是 ERC20 Transfer(address,address,uint256) 事件的签名哈希（topic0）。
const erc20TransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// scanERC20 用 eth_getLogs 拉取代币合约中 to==观察地址的 Transfer 入账，解析 value（最小单位）
// 为 amount。decimals 优先取 w.Decimals，否则按资产名推断（USDT/USDC=6），再否则回退 18。
func (s *JSONRPCDepositScanner) scanERC20(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	toTopic, ok := erc20ToTopic(w.Address)
	if !ok {
		return nil, fmt.Errorf("invalid ERC20 watch address %q", w.Address)
	}
	res, err := s.client.rpc(ctx, ChainETH, "eth_getLogs", []interface{}{
		map[string]interface{}{
			"address": w.Token,
			"topics": []interface{}{
				erc20TransferTopic,
				nil, // from：任意
				toTopic,
			},
			"fromBlock": "0x0",
			"toBlock":   "latest",
		},
	})
	if err != nil {
		return nil, err
	}
	var logs []struct {
		TransactionHash string `json:"transactionHash"`
		Data            string `json:"data"`
	}
	if err := json.Unmarshal(res, &logs); err != nil {
		return nil, err
	}
	decimals := w.Decimals
	if decimals <= 0 {
		decimals = AssetDecimalsByName(w.Asset)
		if decimals <= 0 {
			decimals = 18
		}
	}
	out := make([]DepositEvent, 0, len(logs))
	for _, l := range logs {
		amount := hexToAmount(l.Data, decimals)
		if amount.Sign() <= 0 {
			continue
		}
		out = append(out, DepositEvent{
			TxHash:  strings.Trim(l.TransactionHash, `"`),
			UserID:  w.UserID,
			Asset:   w.Asset,
			Amount:  amount,
			Chain:   ChainETH,
			Address: w.Address,
		})
	}
	return out, nil
}

// erc20ToTopic 把 20 字节地址左补齐为 32 字节 topic 形式（0x + 24个0 + 地址 hex），用于过滤
// Transfer 事件的 to 字段。地址非法时返回 ok=false。
func erc20ToTopic(addr string) (string, bool) {
	a := strings.TrimPrefix(strings.ToLower(addr), "0x")
	if len(a) != 40 {
		return "", false
	}
	return "0x" + strings.Repeat("0", 64-len(a)) + a, true
}

// scanUTXO 用 listsinceblock 拉取 UTXO 链（BTC/LTC/DOGE）地址相关收款并解析 amount（8 位小数）。
// 三者 RPC 方法完全一致，仅按 w.Chain 路由到对应节点端点。
func (s *JSONRPCDepositScanner) scanUTXO(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	res, err := s.client.rpc(ctx, w.Chain, "listsinceblock", []interface{}{})
	if err != nil {
		return nil, err
	}
	var body struct {
		Transactions []struct {
			TxID      string  `json:"txid"`
			Address   string  `json:"address"`
			Amount    float64 `json:"amount"`
			Category  string  `json:"category"`
			Confirmed int     `json:"confirmations"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(res, &body); err != nil {
		return nil, err
	}
	out := make([]DepositEvent, 0)
	for _, t := range body.Transactions {
		amt := AssetAmountFromFloat(t.Amount, 8) // 节点返回 BTC(float)→satoshi 最小单位
		if t.Category != "receive" || t.Address != w.Address || amt.Sign() <= 0 {
			continue
		}
		out = append(out, DepositEvent{
			TxHash:  t.TxID,
			UserID:  w.UserID,
			Asset:   w.Asset,
			Amount:  amt,
			Chain:   w.Chain,
			Address: w.Address,
		})
	}
	return out, nil
}

// weiToAmount 把 0x 十六进制 wei 字符串转为最小单位整数金额（18 decimals，#6，无 float 精度损失）。
func weiToAmount(hex string) AssetAmount {
	return hexToAmount(hex, 18)
}

// hexToAmount 把 0x 十六进制最小单位字符串按给定 decimals 转为 AssetAmount（#6，无 float 精度损失）。
func hexToAmount(hex string, decimals int) AssetAmount {
	h := strings.TrimSpace(strings.TrimPrefix(strings.Trim(hex, `"`), "0x"))
	if h == "" {
		return AssetAmount{}
	}
	v, ok := new(big.Int).SetString(h, 16)
	if !ok {
		return AssetAmount{}
	}
	return AssetAmount{Value: v, Decimals: decimals}
}

// scanTRON 用 TronGrid 风格 REST 拉取观察地址的 TRC20 转账（充值）：按合约地址过滤、
// 仅保留 to==观察地址 的入账，解析 value（最小单位，按 decimals 缩放）为 amount。生产接
// TronGrid 或自建 event 服务；未配 token 时默认按主网 USDT-TRC20 合约过滤。
func (s *JSONRPCDepositScanner) scanTRON(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	token := w.Token
	if token == "" {
		token = tronUSDTContract
	}
	path := fmt.Sprintf("/v1/accounts/%s/transactions/trc20?contract_address=%s&only_confirmed=true", w.Address, token)
	body, err := s.client.get(ctx, ChainTRON, path)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			TransactionID string `json:"transaction_id"`
			TokenInfo     struct {
				Decimals int `json:"decimals"`
			} `json:"token_info"`
			Value string `json:"value"`
			To    string `json:"to"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := make([]DepositEvent, 0, len(resp.Data))
	for _, d := range resp.Data {
		if !strings.EqualFold(d.To, w.Address) {
			continue // 仅保留入账（to==观察地址），转出/其他地址忽略
		}
		// 绝不信任远端 token_info.decimals：攻击者可控的节点响应可伪造 decimals 来放大/缩小金额，
		// 造成盗窃或漏账（F2/F5）。decimals 优先取本地配置 w.Decimals，其次按资产名推断，再否则
		// 回退 6（TRC20 通用；USDT-TRC20 即 6 位小数）。
		decimals := w.Decimals
		if decimals <= 0 {
			decimals = AssetDecimalsByName(w.Asset)
			if decimals <= 0 {
				decimals = 6 // USDT-TRC20 默认 6 位小数（不再信任远端 token_info）
			}
		}
		amount := tronAmountToFloat(d.Value, decimals)
		if amount.Sign() <= 0 {
			continue
		}
		out = append(out, DepositEvent{
			TxHash:  d.TransactionID,
			UserID:  w.UserID,
			Asset:   w.Asset,
			Amount:  amount,
			Chain:   ChainTRON,
			Address: w.Address,
		})
	}
	return out, nil
}

// tronAmountToFloat 把 TRC20 value（字符串，最小单位整数）按 decimals 转为最小单位整数金额（#6）。
func tronAmountToFloat(value string, decimals int) AssetAmount {
	v, ok := new(big.Int).SetString(strings.TrimSpace(value), 10)
	if !ok {
		return AssetAmount{}
	}
	return AssetAmount{Value: v, Decimals: decimals}
}

// RPCDepositGateway 是 DepositGateway 的真实链上 RPC 实现（T-03 链上 RPC 半边·充值回调脚手架）。
// 它嵌入 MockChainGateway，复用其经过验证的确认状态机、孤块/重组回滚与查询能力，仅在「充值
// 来源」上新增一条真实扫描通道（StartScan）：经 DepositScanner 从节点监听入账、喂入确认状态机；
// 未配置扫描器时与 MockChainGateway 行为完全一致（充值仅经 SubmitDeposit 注入）。
type RPCDepositGateway struct {
	*MockChainGateway
	scanner DepositScanner
}

// StartScan 启动真实充值扫描：把扫描器监听到的入账经 SubmitDeposit 喂入确认状态机。ctx
// 取消即停止（与 Server.ctx 联动，随服务退出清理）；无扫描器则为 no-op。
func (g *RPCDepositGateway) StartScan(ctx context.Context) {
	if g.scanner == nil {
		return
	}
	go func() {
		ch, err := g.scanner.Scan(ctx)
		if err != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if _, err := g.SubmitDepositWithHash(ev.UserID, ev.Asset, ev.Chain, ev.Amount, ev.Address, ev.TxHash); err != nil {
					// 非法参数忽略；节点重复推送由 seen 去重，此处不会重复入账。
					continue
				}
			}
		}
	}()
}

// NewDepositGateway 按配置选择链上充值网关实现：启用且配置了 RPC 端点（与观察地址）时返回
// RPCDepositGateway（真实扫描 + 模拟确认），否则返回 MockChainGateway（全模拟）。
func NewDepositGateway(conf ChainRPCConfig) DepositGateway {
	req := conf.Required
	if req <= 0 {
		req = 2
	}
	interval := 2 * time.Second
	if conf.PollSec > 0 {
		interval = time.Duration(conf.PollSec) * time.Second
	}
	if conf.Enabled && len(conf.Endpoints) > 0 && len(conf.WatchAddresses) > 0 {
		client := NewJSONRPCClient(conf.Endpoints)
		mg := NewMockChainGateway(req, interval)
		mg.confirmSource = client // 真实区块确认轮询；节点不可达自动回退模拟
		// 装配 HD 充值地址派生（配置驱动；未配置则 GenerateAddress 回退 mock）。
		ConfigureDepositAddresses(conf.HotWallet.Deposit)
		return &RPCDepositGateway{
			MockChainGateway: mg,
			scanner:          NewJSONRPCDepositScanner(client, conf.WatchAddresses, interval),
		}
	}
	// 纯 mock 网关也尝试装配 HD 充值地址派生（配置驱动）。
	ConfigureDepositAddresses(conf.HotWallet.Deposit)
	return NewMockChainGateway(req, interval)
}
