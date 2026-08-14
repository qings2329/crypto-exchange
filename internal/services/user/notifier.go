package user

import (
	"fmt"
	"strings"
)

// Notifier 负责把验证码投递到用户（邮箱 / 短信）。生产环境应替换为真实
// SMTP / 短信网关实现；dev 用 LogNotifier 把码打到日志以便演示。
type Notifier interface {
	SendCode(target, purpose, code string) error
}

// LogNotifier 把验证码打印到标准输出（开发 / 单测用，不真正发送）。
type LogNotifier struct{}

// NewLogNotifier 构造日志通知器。
func NewLogNotifier() Notifier { return &LogNotifier{} }

// SendCode 打印验证码。
func (n *LogNotifier) SendCode(target, purpose, code string) error {
	channel := "email"
	if isPhone(target) {
		channel = "sms"
	}
	fmt.Printf("[notifier][%s] target=%s purpose=%s code=%s\n", channel, target, purpose, code)
	return nil
}

// isPhone 粗略判断 target 是否为手机号（纯数字，长度 6~15）。
func isPhone(target string) bool {
	if target == "" {
		return false
	}
	for _, r := range target {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(target) >= 6 && len(target) <= 15
}

// isEmail 粗略判断 target 是否为邮箱（含 @）。
func isEmail(target string) bool {
	return strings.Contains(target, "@")
}
