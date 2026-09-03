package matching

import (
	"math"
	"sync"
)

// VIPDiscount 是某一用户等级的交易手续费折扣规则。
// discount 以"扣减比例"表达：discount=0.3 表示在原费率基础上减去 30%，即实收 70%。
// 有效范围 [0, 1)；超出视为 0（完全免佣）。
type VIPDiscount struct {
	Level        int8   `yaml:"level"`         // 用户等级（0=普通，1=VIP1 … 5=VIP5）
	TakerDiscount float64 `yaml:"taker_discount"` // taker 侧扣减比例，如 0.3 代表打七折
	MakerDiscount float64 `yaml:"maker_discount"` // maker 侧扣减比例（通常为 0，即 maker 免手续费或获返佣）
}

// SymbolFeeConfig 是单个交易对的费率覆盖配置。缺省字段（<=0）退回到全局默认。
type SymbolFeeConfig struct {
	TakerRate float64 `yaml:"taker_rate"` // 吃单方费率，如 0.001 = 0.1%；<=0 退全局
	MakerRate float64 `yaml:"maker_rate"` // 挂单方费率；<=0 退全局
}

// TradeFeeModel 是交易手续费模型：支持全局基础费率 + VIP 等级折扣 + 交易对单独覆盖。
// 线程安全：所有读操作均持 RLock；Write 方法持 Lock。
type TradeFeeModel struct {
	mu               sync.RWMutex
	globalTakerRate  float64 // 全局 taker 基础费率（如 0.001）
	globalMakerRate  float64 // 全局 maker 基础费率（如 0.0）
	vipDiscounts     map[int8]VIPDiscount
	symbolOverrides  map[string]SymbolFeeConfig
}

// NewTradeFeeModel 构造手续费模型。baseTaker/baseMaker 以 float 入参（如 0.001）；
// <=0 时分别默认 0.001 / 0.0。注意 baseMaker 可传负值表示 maker 返佣。
func NewTradeFeeModel(baseTaker, baseMaker float64) *TradeFeeModel {
	m := &TradeFeeModel{
		vipDiscounts:    make(map[int8]VIPDiscount),
		symbolOverrides: make(map[string]SymbolFeeConfig),
	}
	if baseTaker > 0 {
		m.globalTakerRate = baseTaker
	} else {
		m.globalTakerRate = 0.001 // 默认 0.1%
	}
	// maker 支持负值（返佣）；baseMaker <=0 且 != 负值时默认 0。
	if baseMaker < 0 {
		m.globalMakerRate = baseMaker
	} else {
		m.globalMakerRate = 0.0
	}
	return m
}

// SetVIPDiscounts 覆盖当前 VIP 折扣表（整表替换）。线程安全。
func (m *TradeFeeModel) SetVIPDiscounts(discounts []VIPDiscount) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vipDiscounts = make(map[int8]VIPDiscount, len(discounts))
	for _, d := range discounts {
		m.vipDiscounts[d.Level] = d
	}
}

// SetSymbolOverrides 覆盖当前交易对费率表（整表替换）。线程安全。
func (m *TradeFeeModel) SetSymbolOverrides(overrides map[string]SymbolFeeConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.symbolOverrides = make(map[string]SymbolFeeConfig, len(overrides))
	for k, v := range overrides {
		m.symbolOverrides[k] = v
	}
}

// SetGlobalRates 修改全局基础费率。线程安全。
func (m *TradeFeeModel) SetGlobalRates(taker, maker float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if taker > 0 {
		m.globalTakerRate = taker
	}
	// maker 允许负值（返佣）；<=0 表示免佣，不变。
	if maker != 0 {
		m.globalMakerRate = maker
	}
}

// TakerRate 计算指定交易对、指定用户等级的 taker 手续费率（返回 0~1 之间的浮点比例，如 0.0007 表示 0.07%）。
// 优先级：交易对覆盖 > VIP 折扣 > 全局默认。
func (m *TradeFeeModel) TakerRate(symbol string, userLevel int8) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	base := m.globalTakerRate
	// 1. 交易对单独覆盖（完全替换，不再叠加 VIP）
	if symCfg, ok := m.symbolOverrides[symbol]; ok && symCfg.TakerRate != 0 {
		return symCfg.TakerRate
	}
	// 2. VIP 折扣
	if disc, ok := m.vipDiscounts[userLevel]; ok {
		discount := math.Min(math.Max(disc.TakerDiscount, 0), 1)
		base = base * (1 - discount)
	}
	return base
}

// MakerRate 计算指定交易对、指定用户等级的 maker 手续费率。
// 同上优先级：交易对覆盖 > VIP 折扣 > 全局默认。
func (m *TradeFeeModel) MakerRate(symbol string, userLevel int8) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	base := m.globalMakerRate
	if symCfg, ok := m.symbolOverrides[symbol]; ok && symCfg.MakerRate > 0 {
		base = symCfg.MakerRate
	}
	if disc, ok := m.vipDiscounts[userLevel]; ok {
		discount := math.Min(math.Max(disc.MakerDiscount, 0), 1)
		base = base * (1 - discount)
	}
	return base
}

// GetSnapshot 返回当前模型的可序列化快照，供 admin API 对外暴露。
func (m *TradeFeeModel) GetSnapshot() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return map[string]interface{}{
		"global_taker_rate":  m.globalTakerRate,
		"global_maker_rate":  m.globalMakerRate,
		"vip_discounts":      toSlice(m.vipDiscounts),
		"symbol_overrides":   m.symbolOverrides,
	}
}

func toSlice(m map[int8]VIPDiscount) []VIPDiscount {
	out := make([]VIPDiscount, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// ComputeTakerFee 把定点成交金额（cost，单位：计价资产最小单位整数）与费率（0~1 浮点）
// 换算为手续费（计价资产最小单位整数）。rate<=0 时返回零金额（免费）。
func ComputeTakerFee(cost int64, rate float64) int64 {
	if rate <= 0 || cost <= 0 {
		return 0
	}
	// 定点乘法：cost * rate，避免浮点漂移。
	// 使用 big.Int 风格：将 rate 放大 1e9 后再乘，最后除回 1e9。
	const scale = 1_000_000_000
	fee := (cost * int64(rate*scale)) / scale
	return fee
}

// GlobalRates 返回当前全局费率快照（供调试）。
func (m *TradeFeeModel) GlobalRates() (taker, maker float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.globalTakerRate, m.globalMakerRate
}
