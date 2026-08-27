package bot

import (
	"fmt"

	"github.com/coldlar/crypto-exchange/internal/oracle"
)

// OraclePriceSource 把 oracle 预言机适配为 bot 的 PriceSource（§39 接真实行情）。
// 当 configs 配置了 oracle.feeds（http 类型指向真实 REST 源，如 Binance/Okx/Coinbase）时，
// bot 网格/定投/MA 策略使用真实指数价驱动下单；未配置则回退到内置静态演示价（与 futures
// 服务同模式的离线回退），保证服务可独立启动。
type OraclePriceSource struct {
	o *oracle.Oracle
}

// NewOraclePriceSource 由 *oracle.Oracle 构造行情源。
func NewOraclePriceSource(o *oracle.Oracle) *OraclePriceSource {
	return &OraclePriceSource{o: o}
}

// Price 取交易对指数价；oracle 无价（未配置/源全部失效）时返回错误，
// 由 tick 逻辑拒绝本轮（F5：非法价不下单）。
func (s *OraclePriceSource) Price(_, symbol string) (float64, error) {
	p, ok := s.o.IndexPrice(symbol)
	if !ok || p <= 0 {
		return 0, fmt.Errorf("bot: no price available for %s", symbol)
	}
	return p, nil
}

// DefaultOracle 构造离线演示用预言机：为常见现货/合约交易对预置静态基准价。
// 仅当 configs 未配置真实喂价源时使用，保证 bot 在无外网环境下也能跑通策略链路。
// 每个交易对配 2 个静态源（满足预言机 MinFeeds=2 聚合要求），价差 0.08% 模拟多源一致性。
func DefaultOracle() *oracle.Oracle {
	base := map[string]float64{
		"BTC_USDT":      50000,
		"ETH_USDT":      3000,
		"BTC_USDT_PERP": 50000,
		"ETH_USDT_PERP": 3000,
	}
	feeds := make(map[string][]oracle.PriceFeed, len(base))
	for sym, p := range base {
		feeds[sym] = []oracle.PriceFeed{
			oracle.NewStaticFeed("binance", p),
			oracle.NewStaticFeed("okx", p*1.0008),
		}
	}
	return oracle.New(oracle.Config{Feeds: feeds})
}
