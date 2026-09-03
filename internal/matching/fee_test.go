package matching

import (
	"testing"
)

func TestNewTradeFeeModelDefaults(t *testing.T) {
	m := NewTradeFeeModel(0, 0) // 全零 → 兜底默认
	taker, maker := m.GlobalRates()
	if taker != 0.001 {
		t.Fatalf("default taker rate = %v, want 0.001", taker)
	}
	if maker != 0.0 {
		t.Fatalf("default maker rate = %v, want 0.0", maker)
	}
}

func TestNewTradeFeeModelExplicit(t *testing.T) {
	m := NewTradeFeeModel(0.0005, -0.0002)
	taker, maker := m.GlobalRates()
	if taker != 0.0005 {
		t.Fatalf("taker rate = %v, want 0.0005", taker)
	}
	if maker != -0.0002 {
		t.Fatalf("maker rate = %v, want -0.0002", maker)
	}
}

func TestTakerRate_Priority(t *testing.T) {
	m := NewTradeFeeModel(0.001, 0.0)
	// 无覆盖、无 VIP → 全局默认
	if r := m.TakerRate("BTC_USDT", 0); r != 0.001 {
		t.Fatalf("default rate = %v, want 0.001", r)
	}
	// VIP0 无折扣 → 仍用全局
	if r := m.TakerRate("BTC_USDT", 0); r != 0.001 {
		t.Fatalf("VIP0 no discount = %v, want 0.001", r)
	}
	// 设置 VIP1 打九折（扣减 0.1）
	m.SetVIPDiscounts([]VIPDiscount{{Level: 1, TakerDiscount: 0.1, MakerDiscount: 0}})
	if r := m.TakerRate("BTC_USDT", 1); r < 0.000899 || r > 0.000901 {
		t.Fatalf("VIP1 rate = %v, want ~0.0009", r)
	}
	// 交易对覆盖优先于 VIP
	m.SetSymbolOverrides(map[string]SymbolFeeConfig{
		"BTC_USDT": {TakerRate: 0.002},
	})
	if r := m.TakerRate("BTC_USDT", 1); r != 0.002 {
		t.Fatalf("symbol override rate = %v, want 0.002 (VIP should not apply)", r)
	}
	// 未覆盖的交易对仍走 VIP
	if r := m.TakerRate("ETH_USDT", 1); r < 0.000899 || r > 0.000901 {
		t.Fatalf("ETH_USDT VIP1 rate = %v, want ~0.0009", r)
	}
}

func TestMakerRate(t *testing.T) {
	m := NewTradeFeeModel(0.001, 0.0) // maker 免费
	if r := m.MakerRate("BTC_USDT", 0); r != 0.0 {
		t.Fatalf("maker rate = %v, want 0.0", r)
	}
	// 负费率 = 返佣
	m.SetGlobalRates(0.001, -0.0005)
	if r := m.MakerRate("BTC_USDT", 0); r < -0.00051 || r > -0.00049 {
		t.Fatalf("maker rebate rate = %v, want ~-0.0005", r)
	}
}

func TestComputeTakerFee(t *testing.T) {
	// 1000 USDT × 0.1% = 1
	if got := ComputeTakerFee(1000, 0.001); got != 1 {
		t.Fatalf("fee = %d, want 1", got)
	}
	// 10000 × 0.05% = 5
	if got := ComputeTakerFee(10000, 0.0005); got != 5 {
		t.Fatalf("fee = %d, want 5", got)
	}
	// 费率为 0 或负 → 免费
	if got := ComputeTakerFee(1000, 0); got != 0 {
		t.Fatalf("zero rate fee = %d, want 0", got)
	}
	if got := ComputeTakerFee(1000, -0.001); got != 0 {
		t.Fatalf("negative rate fee = %d, want 0", got)
	}
	// 金额为 0 → 免费
	if got := ComputeTakerFee(0, 0.001); got != 0 {
		t.Fatalf("zero amount fee = %d, want 0", got)
	}
}

func TestVIPDiscount_Capped(t *testing.T) {
	m := NewTradeFeeModel(0.001, 0.0)
	// discount > 1 应钳位到 1（免费）
	m.SetVIPDiscounts([]VIPDiscount{{Level: 5, TakerDiscount: 2.0}})
	if r := m.TakerRate("BTC_USDT", 5); r != 0 {
		t.Fatalf("capped discount rate = %v, want 0", r)
	}
	// discount < 0 应钳位到 0（无折扣）
	m.SetVIPDiscounts([]VIPDiscount{{Level: 4, TakerDiscount: -0.5}})
	if r := m.TakerRate("BTC_USDT", 4); r != 0.001 {
		t.Fatalf("negative discount rate = %v, want 0.001", r)
	}
}

func TestSnapshot(t *testing.T) {
	m := NewTradeFeeModel(0.002, 0.0)
	m.SetVIPDiscounts([]VIPDiscount{{Level: 2, TakerDiscount: 0.5}})
	m.SetSymbolOverrides(map[string]SymbolFeeConfig{"ETH_USDT": {TakerRate: 0.003}})
 snap := m.GetSnapshot()
	if snap["global_taker_rate"] != 0.002 {
		t.Fatalf("snapshot taker = %v, want 0.002", snap["global_taker_rate"])
	}
	vips, ok := snap["vip_discounts"].([]VIPDiscount)
	if !ok || len(vips) != 1 || vips[0].Level != 2 {
		t.Fatalf("snapshot vip wrong: %v", snap["vip_discounts"])
	}
	sym, ok := snap["symbol_overrides"].(map[string]SymbolFeeConfig)
	if !ok || sym["ETH_USDT"].TakerRate != 0.003 {
		t.Fatalf("snapshot symbol override wrong: %v", snap["symbol_overrides"])
	}
}
