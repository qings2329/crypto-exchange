package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/gagliardetto/solana-go"
)

// solanaUSDCContractMainnet 是 Solana 主网 USDC（SPL）的铸币地址；SPL 观察项/提现未显式
// 指定 mint（DepositWatch.Token / UnsignedTx.ContractAddress）时按资产名取默认 mint。
const solanaUSDCContractMainnet = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// solanaMintByAsset 返回资产对应的 Solana 信息：原生 SOL 标记 isNative=true；SPL 代币（USDC）
// 返回主网 mint。未知资产 isNative=false 且 mint 空（由调用方用 DepositWatch.Token /
// UnsignedTx.ContractAddress 覆盖，或报错）。与 AssetDecimals/KnownAsset 口径一致（F5 白名单）。
func solanaMintByAsset(asset string) (mint string, isNative bool) {
	switch asset {
	case "SOL":
		return "", true
	case "USDC":
		return solanaUSDCContractMainnet, false
	default:
		return "", false
	}
}

// scanSOL 按 Solana 链解析某观察地址的入账（充值）：原生 SOL 用 getSignaturesForAddress +
// getTransaction 的 pre/post Balances 计算余额增量；SPL 代币用 pre/post TokenBalances 计算
// ATA 余额增量。仅成功交易（meta.err==null）且增量为正才产出 DepositEvent；去重交由上层 seen。
func (s *JSONRPCDepositScanner) scanSOL(ctx context.Context, w DepositWatch) ([]DepositEvent, error) {
	mint, isNative := solanaMintByAsset(w.Asset)
	if !isNative && w.Token != "" {
		mint = w.Token // 覆盖 SPL mint（DepositWatch.Token 复用为 SOL 链 mint）
	}
	if !isNative && mint == "" {
		return nil, fmt.Errorf("SPL 资产 %q 缺少 mint（请在 DepositWatch.Token 指定）", w.Asset)
	}
	// 原生 SOL 直接观察 owner 钱包；SPL 观察该 owner 的关联代币账户（ATA）。
	watchAddr := w.Address
	if !isNative {
		owner, err := solana.PublicKeyFromBase58(w.Address)
		if err != nil {
			return nil, fmt.Errorf("SPL 观察地址非法: %w", err)
		}
		mintPub, err := solana.PublicKeyFromBase58(mint)
		if err != nil {
			return nil, fmt.Errorf("SPL mint 非法: %w", err)
		}
		ata, _, err := solana.FindAssociatedTokenAddress(owner, mintPub)
		if err != nil {
			return nil, fmt.Errorf("计算 ATA 失败: %w", err)
		}
		watchAddr = ata.String()
	}

	sigs, err := s.solanaSignaturesForAddress(ctx, watchAddr)
	if err != nil {
		return nil, err
	}
	out := make([]DepositEvent, 0, len(sigs))
	for _, sig := range sigs {
		ev, ok, err := s.solanaParseTransaction(ctx, sig, w, watchAddr, isNative, mint)
		if err != nil {
			continue // 单笔解析失败跳过，不阻断其它
		}
		if !ok {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// solanaSignaturesForAddress 调 getSignaturesForAddress 取涉及该地址的签名列表（成功交易）。
// rpc 方法已返回 result 载荷（数组），此处直接反序列化。
func (s *JSONRPCDepositScanner) solanaSignaturesForAddress(ctx context.Context, addr string) ([]string, error) {
	res, err := s.client.rpc(ctx, ChainSOL, "getSignaturesForAddress", []interface{}{
		addr,
		map[string]interface{}{"encoding": "json", "limit": 1000},
	})
	if err != nil {
		return nil, err
	}
	var sigs []struct {
		Signature string      `json:"signature"`
		Err       interface{} `json:"err"`
	}
	if err := json.Unmarshal(res, &sigs); err != nil {
		return nil, fmt.Errorf("getSignaturesForAddress 解析失败: %w", err)
	}
	out := make([]string, 0, len(sigs))
	for _, r := range sigs {
		if r.Err == nil { // 仅成功交易
			out = append(out, r.Signature)
		}
	}
	return out, nil
}

// solanaParseTransaction 调 getTransaction 解析单笔交易的充值金额；ok=false 表示不计为充值。
// rpc 方法已返回 result 载荷（对象），此处直接反序列化。
func (s *JSONRPCDepositScanner) solanaParseTransaction(ctx context.Context, sig string, w DepositWatch, watchAddr string, isNative bool, mint string) (DepositEvent, bool, error) {
	res, err := s.client.rpc(ctx, ChainSOL, "getTransaction", []interface{}{
		sig,
		map[string]interface{}{"encoding": "json", "maxSupportedTransactionVersion": 0},
	})
	if err != nil {
		return DepositEvent{}, false, err
	}
	var tx struct {
		Meta struct {
			Err               interface{}    `json:"err"`
			PreBalances       []int64        `json:"preBalances"`
			PostBalances      []int64        `json:"postBalances"`
			PreTokenBalances  []tokenBalance `json:"preTokenBalances"`
			PostTokenBalances []tokenBalance `json:"postTokenBalances"`
		} `json:"meta"`
		Transaction struct {
			Message struct {
				AccountKeys []string `json:"accountKeys"`
			} `json:"message"`
		} `json:"transaction"`
	}
	if err := json.Unmarshal(res, &tx); err != nil {
		return DepositEvent{}, false, fmt.Errorf("getTransaction 解析失败: %w", err)
	}
	if tx.Meta.Err != nil { // 失败交易不计充值
		return DepositEvent{}, false, nil
	}

	var amount *big.Int
	if isNative {
		idx := indexOf(tx.Transaction.Message.AccountKeys, watchAddr)
		if idx < 0 || idx >= len(tx.Meta.PostBalances) || idx >= len(tx.Meta.PreBalances) {
			return DepositEvent{}, false, nil
		}
		delta := tx.Meta.PostBalances[idx] - tx.Meta.PreBalances[idx]
		if delta <= 0 {
			return DepositEvent{}, false, nil
		}
		amount = big.NewInt(delta) // lamports（9 decimals）
	} else {
		idx := indexOf(tx.Transaction.Message.AccountKeys, watchAddr)
		if idx < 0 {
			return DepositEvent{}, false, nil
		}
		pre := tokenBalanceAmount(tx.Meta.PreTokenBalances, idx, mint)
		post := tokenBalanceAmount(tx.Meta.PostTokenBalances, idx, mint)
		delta := new(big.Int).Sub(post, pre)
		if delta.Sign() <= 0 {
			return DepositEvent{}, false, nil
		}
		amount = delta // mint 最小单位（USDC 6 decimals）
	}

	return DepositEvent{
		TxHash:  sig,
		UserID:  w.UserID,
		Asset:   w.Asset,
		Amount:  AssetAmount{Value: amount, Decimals: assetDecimalsForSOL(isNative)},
		Chain:   ChainSOL,
		Address: w.Address,
	}, true, nil
}

// tokenBalance 是 getTransaction meta 中单个代币余额快照（ata 索引 + mint + 最小单位整数）。
type tokenBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	UiTokenAmount struct {
		Amount   string `json:"amount"` // 最小单位整数（字符串，避免精度损失）
		Decimals int    `json:"decimals"`
	} `json:"uiTokenAmount"`
}

// tokenBalanceAmount 取某 ata 索引、指定 mint 的余额（最小单位整数）；未找到返回 0。
func tokenBalanceAmount(balances []tokenBalance, idx int, mint string) *big.Int {
	for _, b := range balances {
		if b.AccountIndex == idx && strings.EqualFold(b.Mint, mint) {
			if v, ok := new(big.Int).SetString(strings.TrimSpace(b.UiTokenAmount.Amount), 10); ok {
				return v
			}
		}
	}
	return big.NewInt(0)
}

// assetDecimalsForSOL 返回 Solana 充值解析用的小数位：原生 SOL=9，其余 SPL=6。
func assetDecimalsForSOL(isNative bool) int {
	if isNative {
		return 9
	}
	return 6
}

// indexOf 返回 s 中等于 target 的元素下标；未找到返回 -1。
func indexOf(s []string, target string) int {
	for i, v := range s {
		if v == target {
			return i
		}
	}
	return -1
}

// SolanaRecentBlockHash 调 getLatestBlockhash 取最近区块哈希（交易构造所需）；base58 解码为
// solana.Hash。节点不可达返回错误（由签名器 fail-degraded）。
func (c *JSONRPCClient) SolanaRecentBlockHash(ctx context.Context) (solana.Hash, error) {
	res, err := c.rpc(ctx, ChainSOL, "getLatestBlockhash", []interface{}{})
	if err != nil {
		return solana.Hash{}, err
	}
	var body struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	if err := json.Unmarshal(res, &body); err != nil {
		return solana.Hash{}, fmt.Errorf("getLatestBlockhash 解析失败: %w", err)
	}
	b, err := base58Decode(body.Value.Blockhash)
	if err != nil {
		return solana.Hash{}, fmt.Errorf("blockhash base58 解码失败: %w", err)
	}
	if len(b) != 32 {
		return solana.Hash{}, fmt.Errorf("blockhash 长度异常: %d（应为 32）", len(b))
	}
	var h solana.Hash
	copy(h[:], b)
	return h, nil
}
