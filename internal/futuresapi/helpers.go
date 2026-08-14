package futuresapi

import (
	"fmt"

	"github.com/coldlar/crypto-exchange/internal/futures"
)

// sideName 将持仓方向转为可读字符串。
func sideName(s futures.PosSide) string {
	if s == futures.Long {
		return "long"
	}
	return "short"
}

// parseUserID 解析 user_id 查询参数；非法/缺失返回 0（表示不过滤）。
func parseUserID(s string) int64 {
	var id int64
	if s == "" {
		return 0
	}
	if _, err := fmt.Sscanf(s, "%d", &id); err != nil {
		return 0
	}
	return id
}

// modeName 将保证金模式转为可读字符串。
func modeName(m futures.MarginMode) string {
	if m == futures.Cross {
		return "cross"
	}
	return "isolated"
}
