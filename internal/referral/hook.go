package referral

import (
	"fmt"

	"github.com/coldlar/crypto-exchange/internal/services/user"
)

// HookAdapter 实现 settlement.ReferralRecorder 接口，桥接 user + referral 服务。
type HookAdapter struct {
	userStore  user.Store
	svc        *Service
	commRate   float64 // 佣金比例（如 0.2 = 20%）
}

// NewHookAdapter 构造佣金钩子适配器。
func NewHookAdapter(userStore user.Store, svc *Service, commRate float64) *HookAdapter {
	return &HookAdapter{
		userStore: userStore,
		svc:       svc,
		commRate:  commRate,
	}
}

// GetReferrerByTaker 按用户 ID 查询其邀请人 ID。
func (a *HookAdapter) GetReferrerByTaker(takerID int64) (int64, error) {
	u, err := a.userStore.GetByID(takerID)
	if err != nil {
		return 0, err
	}
	if u.ReferrerID == 0 {
		return 0, nil
	}
	return u.ReferrerID, nil
}

// RecordTradeFee 记录交易佣金。
func (a *HookAdapter) RecordTradeFee(referrerID, takerID int64, asset string, feeAmount int64, bizRef string) error {
	if a.commRate <= 0 {
		return nil
	}
	if referrerID == takerID {
		return fmt.Errorf("referrer cannot be taker")
	}
	return a.svc.RecordTradeCommission(referrerID, takerID, asset, feeAmount, a.commRate, bizRef)
}
