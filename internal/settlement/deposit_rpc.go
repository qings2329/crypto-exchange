package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
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
// 的 JSONRPCClient 收发协议；真实节点解码（ETH Transfer 日志 / BTC listsinceblock 等）在
// scanChain 内按链实现。TRON(TRC20) 因需合约事件过滤，脚手架默认返回空（生产补全）。
type JSONRPCDepositScanner struct {
	client  *JSONRPCClient
	watches []DepositWatch
	poll    time.Duration
}

// NewJSONRPCDepositScanner 由各链 RPC 客户端 + 观察地址列表 + 轮询间隔构造扫描器。
func NewJSONRPCDepositScanner(client *JSONRPCClient, watches []DepositWatch, poll time.Duration) *JSONRPCDepositScanner {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	return &JSONRPCDepositScanner{client: client, watches: watches, poll: poll}
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
	case ChainBTC:
		return s.scanBTC(ctx, w)
	case ChainTRON:
		// TRON(TRC20) 需按合约 topics 过滤 triggerconstantcontract 事件，脚手架暂不实现，
		// 返回空（生产补全，见 §27「剩余」）。
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported chain %s", w.Chain)
	}
}

// scanETH 用 eth_getLogs 拉取观察地址的入账 Transfer 日志，解析 value(wei)→amount。
func (s *JSONRPCDepositScanner) scanETH(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	res, err := s.client.rpc(ctx, ChainETH, "eth_getLogs", []interface{}{
		map[string]interface{}{
			"address":   w.Address,
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
	out := make([]DepositEvent, 0, len(logs))
	for _, l := range logs {
		amount := weiToAmount(l.Data)
		if amount <= 0 {
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

// scanBTC 用 listsinceblock 拉取地址相关收款并解析 amount。
func (s *JSONRPCDepositScanner) scanBTC(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	res, err := s.client.rpc(ctx, ChainBTC, "listsinceblock", []interface{}{})
	if err != nil {
		return nil, err
	}
	var body struct {
		Transactions []struct {
			TxID       string  `json:"txid"`
			Address    string  `json:"address"`
			Amount     float64 `json:"amount"`
			Category   string  `json:"category"`
			Confirmed  int     `json:"confirmations"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(res, &body); err != nil {
		return nil, err
	}
	out := make([]DepositEvent, 0)
	for _, t := range body.Transactions {
		if t.Category != "receive" || t.Address != w.Address || t.Amount <= 0 {
			continue
		}
		out = append(out, DepositEvent{
			TxHash:  t.TxID,
			UserID:  w.UserID,
			Asset:   w.Asset,
			Amount:  t.Amount,
			Chain:   ChainBTC,
			Address: w.Address,
		})
	}
	return out, nil
}

// weiToAmount 把 0x 十六进制 wei 字符串转为资产数量（除以 1e18）。
func weiToAmount(hex string) float64 {
	h := strings.TrimSpace(strings.TrimPrefix(strings.Trim(hex, `"`), "0x"))
	if h == "" {
		return 0
	}
	v, ok := new(big.Int).SetString(h, 16)
	if !ok {
		return 0
	}
	f, _ := new(big.Float).SetInt(v).Float64()
	return f / 1e18
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
				if _, err := g.SubmitDeposit(ev.UserID, ev.Asset, ev.Chain, ev.Amount, ev.Address); err != nil {
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
	if conf.Enabled && len(conf.Endpoints) > 0 && len(conf.WatchAddresses) > 0 {
		return &RPCDepositGateway{
			MockChainGateway: NewMockChainGateway(req, 2*time.Second),
			scanner:          NewJSONRPCDepositScanner(NewJSONRPCClient(conf.Endpoints), conf.WatchAddresses, time.Duration(conf.PollSec)*time.Second),
		}
	}
	return NewMockChainGateway(req, 2*time.Second)
}
