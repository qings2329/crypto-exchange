package referral

import (
	"fmt"
	"time"

	"github.com/coldlar/crypto-exchange/internal/services/user"
)

// DefaultCooldownDays 新用户注册后冷却天数：在此期间被邀请人的交易不产生佣金。
// 防止注册即刷量的推荐刷单行为（§38 referral 冷却）。
const DefaultCooldownDays = 7

// HookAdapter 实现 settlement.ReferralRecorder 接口，桥接 user + referral 服务。
type HookAdapter struct {
	userStore   user.Store
	svc         *Service
	commRate    float64       // 佣金比例（如 0.2 = 20%）
	cooldownDays int          // 冷却天数（0 表示不冷却）
	now         func() time.Time // 可注入当前时间（便于测试）
}

// NewHookAdapter 构造佣金钩子适配器。
func NewHookAdapter(userStore user.Store, svc *Service, commRate float64) *HookAdapter {
	return NewHookAdapterWithCooldown(userStore, svc, commRate, DefaultCooldownDays)
}

// NewHookAdapterWithCooldown 构造带冷却期的佣金钩子适配器。
func NewHookAdapterWithCooldown(userStore user.Store, svc *Service, commRate float64, cooldownDays int) *HookAdapter {
	return &HookAdapter{
		userStore:    userStore,
		svc:          svc,
		commRate:     commRate,
		cooldownDays: cooldownDays,
		now:          time.Now,
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
	// §38 referral 冷却：新用户注册后 N 天内交易不计佣金，防止注册即刷量。
	if a.cooldownDays > 0 {
		taker, err := a.userStore.GetByID(takerID)
		if err == nil && !taker.CreatedAt.IsZero() {
			if a.now().Sub(taker.CreatedAt) < time.Duration(a.cooldownDays)*24*time.Hour {
				return nil // 冷却期内，跳过佣金
			}
		}
	}
	return a.svc.RecordTradeCommission(referrerID, takerID, asset, feeAmount, a.commRate, bizRef)
}
