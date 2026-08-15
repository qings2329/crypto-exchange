// Package spot 是现货交易服务的「领域工具 + HTTP Handler 层」。
//
// 分层定位：
//   - 撮合引擎（internal/matching）是下游领域依赖，负责订单簿与成交流。
//   - 本包负责把撮合引擎装配成可运行服务（成交/深度回调接线、WebSocket 广播），
//     并通过 RegisterRoutes 暴露 /order、/depth、/ws。
//   - cmd/spot/main.go 仅做进程级装配：读配置、建日志、调用 NewServer + RegisterRoutes + Run。
package spot

import (
	"strings"

	"github.com/coldlar/crypto-exchange/internal/matching"
)

// assets 是某交易对拆分出的基础资产与计价资产（如 BTC_USDT → base=BTC, quote=USDT）。
type assets struct {
	base  string
	quote string
}

// splitSymbol 将交易对符号拆为基础/计价资产。现货符号形如 "BTC_USDT"。
func splitSymbol(symbol string) (base, quote string, ok bool) {
	parts := strings.SplitN(symbol, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// freezeRec 记录一笔未完全成交的现货订单所预冻结的资金，便于成交时按笔递减、
// 撤单时释放剩余冻结，避免用户资金被长期占用。
type freezeRec struct {
	user        int64
	side        matching.Side
	symbol      string
	base        string
	quote       string
	frozenQuote float64 // 买方预冻结的计价资产（price*qty），随成交递减
	frozenBase  float64 // 卖方预冻结的基础资产（qty），随成交递减
}

// depthRow 是深度聚合后的单行，便于 JSON 序列化与前端渲染。
type depthRow struct {
	Price  float64 `json:"price"`
	Volume float64 `json:"volume"`
}

// aggregate 将订单簿层级聚合为可序列化的深度行（同价订单量累加）。
func aggregate(levels []matching.Level) []depthRow {
	out := make([]depthRow, 0, len(levels))
	for _, l := range levels {
		var v float64
		for _, o := range l.Orders {
			v += o.Qty
		}
		out = append(out, depthRow{Price: l.Price, Volume: v})
	}
	return out
}
