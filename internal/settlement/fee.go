package settlement

import (
	"fmt"
	"sync"
)

// ChainFee 某链某资产的手续费规则：基础费（固定）+ 费率（占提现额比例）。
// 估算手续费 = Base + Rate*Amount。两者均可为 0（免费）。
type ChainFee struct {
	Base float64
	Rate float64
}

// FeeModel 多链/多资产手续费模型。按 (链, 资产) 维度登记费率，供提现受理时估算手续费，
// 避免上层硬编码费率、并支持不同链/资产差异化定价。线程安全。
type FeeModel struct {
	mu       sync.RWMutex
	schedule map[string]ChainFee // key = "chain:asset"
}

// NewFeeModel 创建空手续费模型。
func NewFeeModel() *FeeModel {
	return &FeeModel{schedule: make(map[string]ChainFee)}
}

func feeKey(chain Chain, asset string) string {
	return fmt.Sprintf("%s:%s", chain, asset)
}

// Register 登记（覆盖）某链某资产的手续费规则。
func (m *FeeModel) Register(chain Chain, asset string, base, rate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedule[feeKey(chain, asset)] = ChainFee{Base: base, Rate: rate}
}

// Lookup 查询某链某资产费率，未登记返回 false。
func (m *FeeModel) Lookup(chain Chain, asset string) (ChainFee, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.schedule[feeKey(chain, asset)]
	return f, ok
}

// Estimate 估算提现手续费：Base + Rate*Amount。未登记该链资产时返回 0（免费），
// 调用方可据此作为默认费率；amount 为负按 0 处理。
func (m *FeeModel) Estimate(chain Chain, asset string, amount float64) float64 {
	f, ok := m.Lookup(chain, asset)
	if !ok {
		return 0
	}
	if amount < 0 {
		amount = 0
	}
	return f.Base + f.Rate*amount
}
