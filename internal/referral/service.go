package referral

import (
	"go.uber.org/zap"
)

// Service 佣金业务逻辑。
type Service struct {
	store Store
	log   *zap.Logger
}

// NewService 构造佣金服务。
func NewService(store Store, log *zap.Logger) *Service {
	return &Service{store: store, log: log}
}

// RecordTradeCommission 记录交易佣金。
// 当 taker（被邀请人）产生交易手续费时，按 rate 给邀请人发放佣金。
// bizRef 用于幂等去重。
func (s *Service) RecordTradeCommission(referrerID, takerID int64, asset string, feeAmount int64, rate float64, bizRef string) error {
	if feeAmount <= 0 {
		return nil // 无手续费则跳过
	}
	if rate <= 0 {
		return nil // 佣金率为 0 则跳过
	}

	commissionAmt := int64(float64(feeAmount) * rate)
	if commissionAmt <= 0 {
		return nil // 计算后为 0 则跳过
	}

	c := &ReferralCommission{
		ReferrerID: referrerID,
		TakerID:    takerID,
		Asset:      asset,
		Amount:     commissionAmt,
		Rate:       rate,
		Status:     CommissionConfirmed, // 直接到账（简化流程）
		BizRef:     bizRef,
	}

	if err := s.store.RecordCommission(c); err != nil {
		if err == ErrCommissionExists {
			s.log.Debug("commission already recorded", zap.String("biz_ref", bizRef))
			return nil // 幂等
		}
		return err
	}

	s.log.Info("referral commission recorded",
		zap.Int64("referrer_id", referrerID),
		zap.Int64("taker_id", takerID),
		zap.String("asset", asset),
		zap.Int64("amount", commissionAmt),
		zap.Float64("rate", rate),
		zap.String("biz_ref", bizRef),
	)
	return nil
}

// GetMyReferralStats 获取用户邀请统计。
func (s *Service) GetMyReferralStats(referrerID int64) (map[string]int64, error) {
	return s.store.TotalByReferrer(referrerID)
}

// ListByReferrer 分页查询佣金记录。
func (s *Service) ListByReferrer(referrerID int64, limit, offset int) ([]*ReferralCommission, int, error) {
	return s.store.ListCommissionsByReferrer(referrerID, limit, offset)
}

// ListAll 管理后台分页查询。
func (s *Service) ListAll(limit, offset int) ([]*ReferralCommission, int, error) {
	return s.store.ListAll(limit, offset)
}
