package referral

import (
	"fmt"
	"time"
)

// CommissionStatus 佣金状态
type CommissionStatus int

const (
	CommissionPending   CommissionStatus = 0 // 待结算
	CommissionConfirmed CommissionStatus = 1 // 已到账
)

func (s CommissionStatus) String() string {
	switch s {
	case CommissionPending:
		return "pending"
	case CommissionConfirmed:
		return "confirmed"
	default:
		return "unknown"
	}
}

// ReferralCommission 佣金记录
type ReferralCommission struct {
	ID         int64            `json:"id"`
	ReferrerID int64            `json:"referrer_id"` // 邀请人（获得佣金）
	TakerID    int64            `json:"taker_id"`    // 被邀请人（产生手续费）
	Asset      string           `json:"asset"`       // 币种（如 USDT）
	Amount     int64            `json:"amount"`      // 佣金金额（最小单位）
	Rate       float64          `json:"rate"`        // 佣金比例（如 0.2 = 20%）
	Status     CommissionStatus `json:"status"`
	BizRef     string           `json:"biz_ref"` // 关联业务 ref（如 spot_trade:xxx）
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// MaxCommissionRate 佣金比例安全上限：佣金不得超过手续费本身，否则平台倒贴。
const MaxCommissionRate = 1.0

// SystemAccount 佣金系统账户定义
var SystemAccountCommission int64 = -15

var (
	ErrCommissionExists  = fmt.Errorf("commission record already exists")
	ErrInvalidRate       = fmt.Errorf("commission rate must be finite and within (0, 1]")
	ErrUnsupportedAsset  = fmt.Errorf("unsupported asset")
)
