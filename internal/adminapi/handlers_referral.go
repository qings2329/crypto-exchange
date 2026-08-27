package adminapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// handleAdminReferralCommissions 查询所有邀请佣金记录（分页）。
func (s *Server) handleAdminReferralCommissions(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	items, total, err := s.referralStore.ListAll(limit, offset)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list referral commissions failed: "+err.Error())
		return
	}
	s.ok(c, gin.H{"commissions": items, "total": total})
}
