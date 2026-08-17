package announcement

import (
	"errors"
	"time"
)

// 公告等级（影响前端展示样式：info 普通、warning 警告、maintenance 维护）。
const (
	LevelInfo        = "info"
	LevelWarning     = "warning"
	LevelMaintenance = "maintenance"
)

// 字段长度上限（与服务端校验、前端约束保持一致）。
const (
	maxTitleLen   = 128
	maxContentLen = 4096
)

// Announcement 是站内公告领域模型。
type Announcement struct {
	ID          int64     `json:"id"`
	Level       string    `json:"level"`        // info / warning / maintenance
	Title       string    `json:"title"`        // 标题（必填）
	Content     string    `json:"content"`      // 正文（可空）
	Active      bool      `json:"active"`       // 是否对外发布
	PublishedAt time.Time `json:"published_at"` // 发布时间（草稿为 0 值）
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AnnouncementInput 是创建/更新公告的入参（字段可选，nil 表示不修改）。
type AnnouncementInput struct {
	Level   *string
	Title   *string
	Content *string
	Active  *bool
}

// 业务错误（调用方据此映射 HTTP 状态码）。
var (
	ErrNotFound       = errors.New("announcement not found")
	ErrInvalidLevel   = errors.New("invalid level (info/warning/maintenance)")
	ErrTitleRequired  = errors.New("title required")
	ErrTitleTooLong   = errors.New("title too long")
	ErrContentTooLong = errors.New("content too long")
)

// validLevel 校验等级是否合法。
func validLevel(l string) bool {
	switch l {
	case LevelInfo, LevelWarning, LevelMaintenance:
		return true
	default:
		return false
	}
}
