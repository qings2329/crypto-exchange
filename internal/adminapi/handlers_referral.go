package adminapi

import (
	"database/sql"
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

	db, err := sql.Open("mysql", s.cfg.MySQL.DSN)
	if err != nil {
		s.fail(c, http.StatusBadGateway, "db unavailable")
		return
	}
	defer db.Close()

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM ce_referral_commissions").Scan(&total)

	rows, err := db.Query(
		`SELECT id, referrer_id, taker_id, asset, amount, rate, status, biz_ref, created_at, updated_at
		 FROM ce_referral_commissions ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type row struct {
		ID         int64   `json:"id"`
		ReferrerID int64   `json:"referrer_id"`
		TakerID    int64   `json:"taker_id"`
		Asset      string  `json:"asset"`
		Amount     int64   `json:"amount"`
		Rate       float64 `json:"rate"`
		Status     int     `json:"status"`
		BizRef     string  `json:"biz_ref"`
		CreatedAt  string  `json:"created_at"`
		UpdatedAt  string  `json:"updated_at"`
	}
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.ReferrerID, &r.TakerID, &r.Asset, &r.Amount, &r.Rate, &r.Status, &r.BizRef, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}
		out = append(out, r)
	}
	s.ok(c, gin.H{"commissions": out, "total": total})
}
