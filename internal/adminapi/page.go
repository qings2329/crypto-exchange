package adminapi

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// parsePage 从查询参数解析分页（limit/offset）。limit 默认 50，上限 500；offset 下限 0。
func parsePage(c *gin.Context) (limit, offset int) {
	limit, _ = strconv.Atoi(c.Query("limit"))
	offset, _ = strconv.Atoi(c.Query("offset"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	return
}

// paginate 对切片做分页，返回当前页与总条数。
func paginate[T any](rows []T, limit, offset int) ([]T, int) {
	total := len(rows)
	if total == 0 {
		return []T{}, 0
	}
	if offset >= total {
		return []T{}, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return rows[offset:end], total
}

// pageEnvelope 把列表包装为 {items, total}，便于前端分页（保持 data 为对象而非裸数组）。
func pageEnvelope[T any](rows []T, limit, offset int) gin.H {
	page, total := paginate(rows, limit, offset)
	return gin.H{"items": page, "total": total}
}
