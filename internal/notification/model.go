package notification

import (
	"errors"
	"time"
)

// 通知类型。其他业务线（user/KYC、risk、ledger 充提）通过 Publish 写入对应类型。
const (
	TypeKYCAproved    = "kyc_approved"
	TypeKYCRejected   = "kyc_rejected"
	TypeRiskAlert     = "risk_alert"
	TypeDepositArrived = "deposit_arrived"
	TypeWithdrawDone  = "withdraw_done"
	TypeSystem        = "system"
)

// 通知状态。
const (
	StatusUnread = "unread"
	StatusRead   = "read"
)

// ErrNotFound 表示通知不存在。
var ErrNotFound = errors.New("notification not found")

// Notification 是一条站内通知。
type Notification struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// validType 校验通知类型是否已知（未知类型也允许写入，避免阻塞调用方）。
func validType(t string) bool {
	switch t {
	case TypeKYCAproved, TypeKYCRejected, TypeRiskAlert, TypeDepositArrived, TypeWithdrawDone, TypeSystem:
		return true
	default:
		return false
	}
}

// PublishInput 是发布一条通知的输入。
type PublishInput struct {
	UserID int64  `json:"user_id"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// LevelOf 把内部通知类型映射为前端展示等级（info | warning | critical）。
// 前端 UserNotification.level 取值见 crypto-exchange-web/src/api/client.ts。
func LevelOf(t string) string {
	switch t {
	case TypeRiskAlert:
		return "critical"
	case TypeKYCRejected:
		return "warning"
	default:
		return "info"
	}
}
