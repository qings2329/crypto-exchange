package adminapi

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/c2c"
)

// handleC2COrders 分页查询全部 C2C 订单（运营/风控）。
// 支持过滤：user_id、side、coin、status；分页 limit/offset。响应 {orders, total}。
func (s *Server) handleC2COrders(c *gin.Context) {
	limit, offset := parsePage(c)
	filter := c2c.OrderFilter{
		UserID: atoi64(c.Query("user_id")),
		Side:   c2c.Side(c.Query("side")),
		Coin:   c.Query("coin"),
		Status: c2c.OrderStatus(c.Query("status")),
	}
	items, total, err := s.c2cSvc.List(filter, limit, offset)
	if err != nil {
		s.fail(c, http.StatusBadRequest, err.Error())
		return
	}
	s.ok(c, gin.H{"orders": items, "total": total})
}

// handleC2COrderAction 处理 C2C 订单的运营动作（冻结/解冻/完成）。
func (s *Server) handleC2COrderAction(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		s.fail(c, http.StatusBadRequest, "bad order id")
		return
	}
	action := c.Param("action")
	var o *c2c.Order
	switch action {
	case "freeze":
		o, err = s.c2cSvc.Freeze(id)
	case "release":
		o, err = s.c2cSvc.Release(id)
	case "complete":
		o, err = s.c2cSvc.Complete(id)
	default:
		s.fail(c, http.StatusBadRequest, "unknown action: "+action)
		return
	}
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, c2c.ErrNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, c2c.ErrBadTransition) {
			code = http.StatusConflict
		}
		s.fail(c, code, err.Error())
		return
	}
	s.ok(c, gin.H{"order": o, "action": action})
}

func atoi64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// SeedDemoC2COrders 幂等注入若干 C2C 演示订单，供「C2C 管理」页有真实数据可看。
// 由 cmd/admin 启动时显式调用（连库模式按用户/币/方向判重，重启不重复插入）。
func (s *Server) SeedDemoC2COrders() {
	n, err := s.c2cSvc.SeedDemo()
	if err != nil {
		// 记录日志但不中断启动
		log.Printf("[admin] seed demo c2c orders failed: %v", err)
		return
	}
	log.Printf("[admin] seeded demo c2c orders (%d)", n)
}
