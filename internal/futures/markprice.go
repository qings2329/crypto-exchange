package futures

import "sync"

// MarkPriceAlpha 标记价格溢价指数 EMA 平滑系数（演示 1/8）。
// 值越小越平滑、抗插针能力越强，但跟随真实价格越慢。
const MarkPriceAlpha = 1.0 / 8.0

// MarkPriceCalculator 独立标记价格计算器。
//
// 标记价格 = 指数价 + EMA(合约成交流 − 指数价)
//          = 指数价 × (1 + 溢价指数EMA)
//
// 与裸用成交流（t.Price）相比，EMA 平滑后：
//  1. 单笔插针单（瞬时极端成交价）只会轻微扰动标记价格，避免误触强平。
//  2. 资金费率基于同一平滑标记价计算，价格更公允。
//
// 生产环境指数价应来自多交易所现货加权/预言机；此处合约成交流作为
// 溢价 EMA 的输入（真实交易所常用 (合约中间价 − 指数价) 的 EMA）。
type MarkPriceCalculator struct {
	mu       sync.RWMutex
	index    float64 // 指数价（外部喂入，如预言机）
	emaBasis float64 // EMA(合约成交流 − 指数价)
	inited   bool
	alpha    float64
}

// NewMarkPriceCalculator 创建标记价格计算器；alpha 传 0 使用默认 1/8。
func NewMarkPriceCalculator(alpha float64) *MarkPriceCalculator {
	if alpha <= 0 {
		alpha = MarkPriceAlpha
	}
	return &MarkPriceCalculator{alpha: alpha}
}

// SetIndex 设置指数价（由预言机/多交易所现货加权喂入）。
func (m *MarkPriceCalculator) SetIndex(index float64) {
	m.mu.Lock()
	m.index = index
	m.mu.Unlock()
}

// UpdateContractPrice 用一笔合约成交价更新溢价 EMA。
// 在每次成交流回调时调用；指数价应提前通过 SetIndex 设置。
func (m *MarkPriceCalculator) UpdateContractPrice(contract float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	basis := contract - m.index
	if !m.inited {
		m.emaBasis = basis
		m.inited = true
		return
	}
	m.emaBasis = m.emaBasis*(1-m.alpha) + basis*m.alpha
}

// MarkPrice 当前标记价格 = 指数价 + 溢价EMA。
func (m *MarkPriceCalculator) MarkPrice() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.index + m.emaBasis
}

// PremiumEMA 溢价指数 EMA = (标记价 − 指数价) / 指数价，供资金费率计算。
func (m *MarkPriceCalculator) PremiumEMA() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.index <= 0 {
		return 0
	}
	return m.emaBasis / m.index
}
