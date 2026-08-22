package settlement

import (
	"fmt"
	"math/big"
	"sync"
)

// ChainFee 某链某资产的手续费规则：基础费（固定）+ 费率（占提现额比例）。
// 估算手续费 = Base + Rate*Amount。两者均可为 0（免费）。金额用 AssetAmount（最小单位整数，#6）。
type ChainFee struct {
	Base AssetAmount
	Rate *big.Rat
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

// Register 登记（覆盖）某链某资产的手续费规则。base/rate 以 float 入参，内部转为
// AssetAmount（按链标准 decimals）与 *big.Rat，保持调用方（futuresapi）无需改动。
func (m *FeeModel) Register(chain Chain, asset string, base, rate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedule[feeKey(chain, asset)] = ChainFee{
		// 保留裸 FromFloat：base 取自可信配置（Register 由装配时调用，非用户请求），NaN/Inf 不可达；
		// rate 经 *big.Rat.SetFloat64 解析，异常值在此场景下等同费率 0，已由上层配置保证有效。
		Base: AssetAmountFromFloat(base, AssetDecimals(chain, asset)),
		Rate: new(big.Rat).SetFloat64(rate),
	}
}

// Lookup 查询某链某资产费率，未登记返回 false。
func (m *FeeModel) Lookup(chain Chain, asset string) (ChainFee, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.schedule[feeKey(chain, asset)]
	return f, ok
}

// Estimate 估算提现手续费：Base + Rate*Amount（按 amount.Decimals 对齐）。未登记该链资产时
// 返回零金额（免费）；amount 为负按 0 处理。
func (m *FeeModel) Estimate(chain Chain, asset string, amount AssetAmount) AssetAmount {
	f, ok := m.Lookup(chain, asset)
	if !ok {
		return AssetAmount{}
	}
	if amount.Sign() < 0 {
		amount = AssetAmount{}
	}
	dec := amount.Decimals
	base := f.Base.toDecimals(dec)
	fee := new(big.Int).Set(base.Value)
	if f.Rate != nil && f.Rate.Sign() != 0 {
		scaled := new(big.Int).Mul(amount.Value, f.Rate.Num())
		scaled.Quo(scaled, f.Rate.Denom())
		fee.Add(fee, scaled)
	}
	return AssetAmount{Value: fee, Decimals: dec}
}
