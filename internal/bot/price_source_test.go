package bot

import (
	"testing"

	"github.com/coldlar/crypto-exchange/internal/oracle"
	"go.uber.org/zap"
)

// TestOraclePriceSource 验证 §39 适配器：把 oracle 指数价透传给 bot PriceSource。
func TestOraclePriceSource(t *testing.T) {
	o := oracle.New(oracle.Config{
		Feeds: map[string][]oracle.PriceFeed{
			"BTC_USDT": {
				oracle.NewStaticFeed("binance", 50000),
				oracle.NewStaticFeed("okx", 50000),
			},
			"BTC_USDT_PERP": {
				oracle.NewStaticFeed("binance", 51000),
				oracle.NewStaticFeed("okx", 51000),
			},
		},
	})
	o.Start()
	defer o.Stop()

	src := NewOraclePriceSource(o)

	// 已配置交易对返回真实指数价。
	p, err := src.Price("spot", "BTC_USDT")
	if err != nil {
		t.Fatalf("BTC_USDT price err: %v", err)
	}
	if p != 50000 {
		t.Fatalf("BTC_USDT price = %v, want 50000", p)
	}
	p, err = src.Price("futures", "BTC_USDT_PERP")
	if err != nil {
		t.Fatalf("BTC_USDT_PERP price err: %v", err)
	}
	if p != 51000 {
		t.Fatalf("BTC_USDT_PERP price = %v, want 51000", p)
	}

	// 未配置交易对返回错误（F5：tick 拒绝本轮）。
	if _, err := src.Price("spot", "DOGE_USDT"); err == nil {
		t.Fatal("expected error for unconfigured symbol")
	}
}

// TestDefaultOracle 验证离线演示预言机可驱动 bot 链路。
func TestDefaultOracle(t *testing.T) {
	o := DefaultOracle()
	o.Start()
	defer o.Stop()
	src := NewOraclePriceSource(o)
	for _, sym := range []string{"BTC_USDT", "ETH_USDT", "BTC_USDT_PERP", "ETH_USDT_PERP"} {
		if p, err := src.Price("spot", sym); err != nil || p <= 0 {
			t.Fatalf("default oracle %s: price=%v err=%v", sym, p, err)
		}
	}
}

// TestServiceUsesOraclePrice 验证机器人服务在注入 OraclePriceSource 时，
// tick 使用真实指数价计算下单数量（而非写死的 MockPrice=100）。
func TestServiceUsesOraclePrice(t *testing.T) {
	o := oracle.New(oracle.Config{
		Feeds: map[string][]oracle.PriceFeed{
			"BTC_USDT": {
				oracle.NewStaticFeed("binance", 40000),
				oracle.NewStaticFeed("okx", 40000),
			},
		},
	})
	o.Start()
	defer o.Stop()

	store := NewMemStore()
	exec := &mockExec{}
	svc := NewService(store, NewOraclePriceSource(o), exec, Config{}, zap.NewNop())

	st := &BotStrategy{
		ID: 1, UserID: 1, Market: MarketSpot, Symbol: "BTC_USDT", Side: "buy",
		Type: StrategyDCA, Status: StrategyActive, UserToken: "tok",
		Params: BotParams{OrderAmount: 4000, DCAIntervalSec: 60, DCAAmount: 4000},
	}
	if err := store.CreateStrategy(st); err != nil {
		t.Fatal(err)
	}
	if err := svc.tick(st); err != nil {
		t.Fatalf("tick err: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("expected 1 order, got %d", len(exec.calls))
	}
	// qty = OrderAmount / price = 4000 / 40000 = 0.1（若用 MockPrice=100 则为 40，明显不同）。
	qty := exec.calls[0].qty
	if qty < 0.09 || qty > 0.11 {
		t.Fatalf("qty = %v, expected ~0.1 (real oracle price), not 40 (mock)", qty)
	}
}
