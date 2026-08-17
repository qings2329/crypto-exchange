package wealth

import (
	"database/sql"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// mysqlStore 是 MySQL 版 Store。表名 ce_wealth_products / ce_wealth_holdings 已遵守 ce_ 前缀约定。
type mysqlStore struct {
	db *sql.DB
}

func (s *mysqlStore) Close() error {
	return s.db.Close()
}

// --- 产品 ---

func (s *mysqlStore) CreateProduct(p *WealthProduct) error {
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	res, err := s.db.Exec(`
		INSERT INTO ce_wealth_products
			(name, asset, type, annual_rate, duration_days, min_amount, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Asset, string(p.Type), p.AnnualRate, p.DurationDays, p.MinAmount,
		string(p.Status), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		p.ID = id
	}
	return nil
}

func (s *mysqlStore) GetProduct(id int64) (*WealthProduct, error) {
	row := s.db.QueryRow(`
		SELECT id, name, asset, type, annual_rate, duration_days, min_amount, status, created_at, updated_at
		FROM ce_wealth_products WHERE id = ?`, id)
	return scanProduct(row)
}

func (s *mysqlStore) ListProducts(status ProductStatus) ([]*WealthProduct, error) {
	q := `
		SELECT id, name, asset, type, annual_rate, duration_days, min_amount, status, created_at, updated_at
		FROM ce_wealth_products`
	args := []interface{}{}
	if status != "" {
		q += " WHERE status = ?"
		args = append(args, string(status))
	}
	q += " ORDER BY id"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProducts(rows)
}

func (s *mysqlStore) UpdateProduct(p *WealthProduct) error {
	p.UpdatedAt = time.Now()
	_, err := s.db.Exec(`
		UPDATE ce_wealth_products SET
			name=?, asset=?, type=?, annual_rate=?, duration_days=?, min_amount=?, status=?, updated_at=?
		WHERE id = ?`,
		p.Name, p.Asset, string(p.Type), p.AnnualRate, p.DurationDays, p.MinAmount,
		string(p.Status), p.UpdatedAt, p.ID)
	return err
}

// --- 持仓 ---

func (s *mysqlStore) CreateHolding(h *WealthHolding) error {
	now := time.Now()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	if h.LastAccrualAt.IsZero() {
		h.LastAccrualAt = now
	}
	h.UpdatedAt = now
	res, err := s.db.Exec(`
		INSERT INTO ce_wealth_holdings
			(user_id, product_id, asset, principal, accrued_yield, status, created_at, last_accrual_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.UserID, h.ProductID, h.Asset, h.Principal.HumanString(), h.AccruedYield.HumanString(),
		string(h.Status), h.CreatedAt, h.LastAccrualAt, h.UpdatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		h.ID = id
	}
	return nil
}

func (s *mysqlStore) GetHolding(id int64) (*WealthHolding, error) {
	row := s.db.QueryRow(`
		SELECT id, user_id, product_id, asset, principal, accrued_yield, status, created_at, last_accrual_at, redeemed_at, updated_at
		FROM ce_wealth_holdings WHERE id = ?`, id)
	return scanHolding(row)
}

func (s *mysqlStore) UpdateHolding(h *WealthHolding) error {
	h.UpdatedAt = time.Now()
	_, err := s.db.Exec(`
		UPDATE ce_wealth_holdings SET
			user_id=?, product_id=?, asset=?, principal=?, accrued_yield=?, status=?, created_at=?, last_accrual_at=?, redeemed_at=?, updated_at=?
		WHERE id = ?`,
		h.UserID, h.ProductID, h.Asset, h.Principal.HumanString(), h.AccruedYield.HumanString(), string(h.Status),
		h.CreatedAt, h.LastAccrualAt, toNullTime(h.RedeemedAt), h.UpdatedAt, h.ID)
	return err
}

func (s *mysqlStore) DeleteHolding(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ce_wealth_holdings WHERE id = ?`, id)
	return err
}

func (s *mysqlStore) ListHoldings(userID int64) ([]*WealthHolding, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, product_id, asset, principal, accrued_yield, status, created_at, last_accrual_at, redeemed_at, updated_at
		FROM ce_wealth_holdings WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHoldings(rows)
}

func (s *mysqlStore) ListAllHoldings() ([]*WealthHolding, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, product_id, asset, principal, accrued_yield, status, created_at, last_accrual_at, redeemed_at, updated_at
		FROM ce_wealth_holdings ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHoldings(rows)
}

// toNullTime 将零值 time.Time 转为 NULL（避免写入 '0000-00-00' 被 NO_ZERO_DATE 拒绝）。
func toNullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// --- 扫描辅助 ---

func scanProduct(row *sql.Row) (*WealthProduct, error) {
	var p WealthProduct
	var typ, status string
	err := row.Scan(&p.ID, &p.Name, &p.Asset, &typ, &p.AnnualRate, &p.DurationDays,
		&p.MinAmount, &status, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrProductNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Type = ProductType(typ)
	p.Status = ProductStatus(status)
	return &p, nil
}

func scanProducts(rows *sql.Rows) ([]*WealthProduct, error) {
	out := make([]*WealthProduct, 0)
	for rows.Next() {
		var p WealthProduct
		var typ, status string
		if err := rows.Scan(&p.ID, &p.Name, &p.Asset, &typ, &p.AnnualRate, &p.DurationDays,
			&p.MinAmount, &status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Type = ProductType(typ)
		p.Status = ProductStatus(status)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// parseWealthAmount 把存储的 principal/accrued_yield 字符串（AssetAmount.HumanString）按持仓资产
// 的小数位解析为 AssetAmount。
func parseWealthAmount(s, asset string) settlement.AssetAmount {
	dec := settlement.AssetDecimalsByName(asset)
	aa, err := settlement.AssetAmountFromString(s, dec)
	if err != nil {
		return settlement.AssetAmount{Decimals: dec}
	}
	return aa.ToDecimals(dec)
}

func scanHolding(row *sql.Row) (*WealthHolding, error) {
	var h WealthHolding
	var status, asset, principalStr, accruedStr string
	var redeemedAt sql.NullTime
	err := row.Scan(&h.ID, &h.UserID, &h.ProductID, &asset, &principalStr, &accruedStr,
		&status, &h.CreatedAt, &h.LastAccrualAt, &redeemedAt, &h.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrHoldingNotFound
	}
	if err != nil {
		return nil, err
	}
	h.Asset = asset
	h.Principal = parseWealthAmount(principalStr, asset)
	h.AccruedYield = parseWealthAmount(accruedStr, asset)
	h.Status = HoldingStatus(status)
	if redeemedAt.Valid {
		h.RedeemedAt = redeemedAt.Time
	}
	return &h, nil
}

func scanHoldings(rows *sql.Rows) ([]*WealthHolding, error) {
	out := make([]*WealthHolding, 0)
	for rows.Next() {
		var h WealthHolding
		var status, asset, principalStr, accruedStr string
		var redeemedAt sql.NullTime
		if err := rows.Scan(&h.ID, &h.UserID, &h.ProductID, &asset, &principalStr, &accruedStr,
			&status, &h.CreatedAt, &h.LastAccrualAt, &redeemedAt, &h.UpdatedAt); err != nil {
			return nil, err
		}
		h.Asset = asset
		h.Principal = parseWealthAmount(principalStr, asset)
		h.AccruedYield = parseWealthAmount(accruedStr, asset)
		h.Status = HoldingStatus(status)
		if redeemedAt.Valid {
			h.RedeemedAt = redeemedAt.Time
		}
		out = append(out, &h)
	}
	return out, rows.Err()
}
