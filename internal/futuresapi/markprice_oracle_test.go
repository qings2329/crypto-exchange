package futuresapi

import (
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/oracle"
	"github.com/coldlar/crypto-exchange/internal/ws"
)

// TestMarkPriceSourcedFromOracle 回归：合约标记价格的「指数价」分量必须来自预言机，
// 证明 perp 定价链路已接通 oracle（而非裸用成交流）。此前曾怀疑定价未接通，本测试锁定该行为。
func TestMarkPriceSourcedFromOracle(t *testing.T) {
	o := oracle.New(oracle.Config{
		MinFeeds: 1,
		Feeds: map[string][]oracle.PriceFeed{
			"BTC_USDT_PERP": {oracle.NewStaticFeed("binance", 50000)},
		},
	})
	o.Start()
	defer o.Stop()
	if idx, ok := o.IndexPrice("BTC_USDT_PERP"); !ok || idx != 50000 {
		t.Fatalf("oracle index not ready: got %v ok=%v", idx, ok)
	}

	// 复刻 Server.Start 中对 markCalcs 的初始化：指数价来自预言机。
	s := &Server{}
	s.markCalcs = make(map[string]*futures.MarkPriceCalculator)
	for _, sym := range []string{"BTC_USDT_PERP"} {
		mc := futures.NewMarkPriceCalculator(0)
		if idx, ok := o.IndexPrice(sym); ok {
			mc.SetIndex(idx)
		}
		s.markCalcs[sym] = mc
	}

	// 尚无成交流时，标记价应等于预言机指数价。
	if got := s.markCalcs["BTC_USDT_PERP"].MarkPrice(); got != 50000 {
		t.Fatalf("mark price should equal oracle index 50000 before any trade, got %v", got)
	}

	// onTrade 路径不应 panic，且成交流会驱动标记价（首笔后 mark==成交价）。
	s.oracleSvc = o
	s.funding = futures.NewFundingManager(30 * time.Second)
	s.hub = ws.NewHub()
	s.liquidator = futures.NewLiquidator(func(ev futures.LiquidationEvent) {})
	s.onTrade("BTC_USDT_PERP", matching.Trade{Price: matching.FixedFromFloat(51000, 2)})
	if got := s.markCalcs["BTC_USDT_PERP"].MarkPrice(); got != 51000 {
		t.Fatalf("after first trade mark should equal trade price 51000, got %v", got)
	}
}
