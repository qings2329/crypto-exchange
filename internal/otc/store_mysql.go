package otc

import (
	"database/sql"
	"math/big"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// mysqlStore 是 MySQL 版 Store。表名 ce_otc_advertisements / ce_otc_orders / ce_otc_counterparties 已遵守 ce_ 前缀约定。
type mysqlStore struct {
	db *sql.DB
}

func (s *mysqlStore) Close() error {
	return s.db.Close()
}

// --- 广告 ---

func (s *mysqlStore) CreateAd(a *OtcAdvertisement) error {
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	res, err := s.db.Exec(`
		INSERT INTO ce_otc_advertisements
			(user_id, side, asset, fiat_currency, price, min_amount, max_amount, payment_methods, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.UserID, string(a.Side), a.Asset, a.FiatCurrency, a.Price, a.MinAmount, a.MaxAmount,
		a.PaymentMethods, string(a.Status), a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		a.ID = id
	}
	return nil
}

func (s *mysqlStore) GetAd(id int64) (*OtcAdvertisement, error) {
	row := s.db.QueryRow(`
		SELECT id, user_id, side, asset, fiat_currency, price, min_amount, max_amount, payment_methods, status, created_at, updated_at
		FROM ce_otc_advertisements WHERE id = ?`, id)
	return scanAd(row)
}

func (s *mysqlStore) ListAds(side AdSide, asset string) ([]*OtcAdvertisement, error) {
	q := `
		SELECT id, user_id, side, asset, fiat_currency, price, min_amount, max_amount, payment_methods, status, created_at, updated_at
		FROM ce_otc_advertisements WHERE status = ?`
	args := []interface{}{string(AdOpen)}
	if side != "" {
		q += " AND side = ?"
		args = append(args, string(side))
	}
	if asset != "" {
		q += " AND asset = ?"
		args = append(args, asset)
	}
	q += " ORDER BY id"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAds(rows)
}

func (s *mysqlStore) UpdateAd(a *OtcAdvertisement) error {
	a.UpdatedAt = time.Now()
	_, err := s.db.Exec(`
		UPDATE ce_otc_advertisements SET
			side=?, asset=?, fiat_currency=?, price=?, min_amount=?, max_amount=?, payment_methods=?, status=?, updated_at=?
		WHERE id = ?`,
		string(a.Side), a.Asset, a.FiatCurrency, a.Price, a.MinAmount, a.MaxAmount,
		a.PaymentMethods, string(a.Status), a.UpdatedAt, a.ID)
	return err
}

// --- 订单 ---

func (s *mysqlStore) CreateOrder(o *OtcOrder) error {
	now := time.Now()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	o.UpdatedAt = now
	res, err := s.db.Exec(`
		INSERT INTO ce_otc_orders
			(ad_id, maker_id, taker_id, side, asset, fiat_currency, crypto_amount, price, fiat_amount, payment_method, status, rating, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.AdID, o.MakerID, o.TakerID, string(o.Side), o.Asset, o.FiatCurrency, o.CryptoAmount.HumanString(),
		o.Price, o.FiatAmount, o.PaymentMethod, string(o.Status), o.Rating, o.CreatedAt, o.UpdatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		o.ID = id
	}
	return nil
}

func (s *mysqlStore) GetOrder(id int64) (*OtcOrder, error) {
	row := s.db.QueryRow(`
		SELECT id, ad_id, maker_id, taker_id, side, asset, fiat_currency, crypto_amount, price, fiat_amount, payment_method, status, rating, created_at, paid_at, completed_at, updated_at
		FROM ce_otc_orders WHERE id = ?`, id)
	return scanOrder(row)
}

func (s *mysqlStore) UpdateOrder(o *OtcOrder) error {
	o.UpdatedAt = time.Now()
	_, err := s.db.Exec(`
		UPDATE ce_otc_orders SET
			ad_id=?, maker_id=?, taker_id=?, side=?, asset=?, fiat_currency=?, crypto_amount=?, price=?, fiat_amount=?,
			payment_method=?, status=?, rating=?, paid_at=?, completed_at=?, updated_at=?
		WHERE id = ?`,
		o.AdID, o.MakerID, o.TakerID, string(o.Side), o.Asset, o.FiatCurrency, o.CryptoAmount.HumanString(), o.Price,
		o.FiatAmount, o.PaymentMethod, string(o.Status), o.Rating,
		toNullTime(o.PaidAt), toNullTime(o.CompletedAt), o.UpdatedAt, o.ID)
	return err
}

// toNullTime 将零值 time.Time 转为 NULL（避免写入 '0000-00-00' 被 NO_ZERO_DATE 拒绝）。
func toNullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

func (s *mysqlStore) ListOrders(userID int64) ([]*OtcOrder, error) {
	rows, err := s.db.Query(`
		SELECT id, ad_id, maker_id, taker_id, side, asset, fiat_currency, crypto_amount, price, fiat_amount, payment_method, status, rating, created_at, paid_at, completed_at, updated_at
		FROM ce_otc_orders WHERE maker_id = ? OR taker_id = ? ORDER BY id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func (s *mysqlStore) ListAllOrders() ([]*OtcOrder, error) {
	rows, err := s.db.Query(`
		SELECT id, ad_id, maker_id, taker_id, side, asset, fiat_currency, crypto_amount, price, fiat_amount, payment_method, status, rating, created_at, paid_at, completed_at, updated_at
		FROM ce_otc_orders ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// --- 对手方信用 ---

func (s *mysqlStore) UpsertCounterparty(cp *OtcCounterparty) error {
	now := time.Now()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	if cp.ID == 0 {
		res, err := s.db.Exec(`
			INSERT INTO ce_otc_counterparties
				(user_id, counterparty_id, trades_total, trades_completed, rating_sum, rating_count, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			cp.UserID, cp.CounterpartyID, cp.TradesTotal, cp.TradesCompleted, cp.RatingSum, cp.RatingCount, cp.CreatedAt, cp.UpdatedAt)
		if err != nil {
			return err
		}
		if id, err := res.LastInsertId(); err == nil {
			cp.ID = id
		}
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO ce_otc_counterparties
			(id, user_id, counterparty_id, trades_total, trades_completed, rating_sum, rating_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			trades_total=VALUES(trades_total), trades_completed=VALUES(trades_completed),
			rating_sum=VALUES(rating_sum), rating_count=VALUES(rating_count), updated_at=VALUES(updated_at)`,
		cp.ID, cp.UserID, cp.CounterpartyID, cp.TradesTotal, cp.TradesCompleted, cp.RatingSum, cp.RatingCount, cp.CreatedAt, cp.UpdatedAt)
	return err
}

func (s *mysqlStore) GetCounterparty(userID, counterpartyID int64) (*OtcCounterparty, error) {
	row := s.db.QueryRow(`
		SELECT id, user_id, counterparty_id, trades_total, trades_completed, rating_sum, rating_count, created_at, updated_at
		FROM ce_otc_counterparties WHERE user_id = ? AND counterparty_id = ?`, userID, counterpartyID)
	return scanCounterparty(row)
}

func (s *mysqlStore) ListCounterparties(userID int64) ([]*OtcCounterparty, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, counterparty_id, trades_total, trades_completed, rating_sum, rating_count, created_at, updated_at
		FROM ce_otc_counterparties WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCounterparties(rows)
}

// --- 扫描辅助 ---

func scanAd(row *sql.Row) (*OtcAdvertisement, error) {
	var a OtcAdvertisement
	var side, status string
	err := row.Scan(&a.ID, &a.UserID, &side, &a.Asset, &a.FiatCurrency, &a.Price,
		&a.MinAmount, &a.MaxAmount, &a.PaymentMethods, &status, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrAdNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Side = AdSide(side)
	a.Status = AdStatus(status)
	return &a, nil
}

func scanAds(rows *sql.Rows) ([]*OtcAdvertisement, error) {
	out := make([]*OtcAdvertisement, 0)
	for rows.Next() {
		var a OtcAdvertisement
		var side, status string
		if err := rows.Scan(&a.ID, &a.UserID, &side, &a.Asset, &a.FiatCurrency, &a.Price,
			&a.MinAmount, &a.MaxAmount, &a.PaymentMethods, &status, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Side = AdSide(side)
		a.Status = AdStatus(status)
		out = append(out, &a)
	}
	return out, rows.Err()
}

func scanOrder(row *sql.Row) (*OtcOrder, error) {
	var o OtcOrder
	var side, status string
	var cryptoAmount string
	var paidAt, completedAt sql.NullTime
	err := row.Scan(&o.ID, &o.AdID, &o.MakerID, &o.TakerID, &side, &o.Asset, &o.FiatCurrency,
		&cryptoAmount, &o.Price, &o.FiatAmount, &o.PaymentMethod, &status, &o.Rating,
		&o.CreatedAt, &paidAt, &completedAt, &o.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	o.Side = AdSide(side)
	o.Status = OrderStatus(status)
	o.CryptoAmount = parseCryptoAmount(cryptoAmount, o.Asset)
	if paidAt.Valid {
		o.PaidAt = paidAt.Time
	}
	if completedAt.Valid {
		o.CompletedAt = completedAt.Time
	}
	return &o, nil
}

func scanOrders(rows *sql.Rows) ([]*OtcOrder, error) {
	out := make([]*OtcOrder, 0)
	for rows.Next() {
		var o OtcOrder
		var side, status string
		var cryptoAmount string
		var paidAt, completedAt sql.NullTime
		if err := rows.Scan(&o.ID, &o.AdID, &o.MakerID, &o.TakerID, &side, &o.Asset, &o.FiatCurrency,
			&cryptoAmount, &o.Price, &o.FiatAmount, &o.PaymentMethod, &status, &o.Rating,
			&o.CreatedAt, &paidAt, &completedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		o.Side = AdSide(side)
		o.Status = OrderStatus(status)
		o.CryptoAmount = parseCryptoAmount(cryptoAmount, o.Asset)
		if paidAt.Valid {
			o.PaidAt = paidAt.Time
		}
		if completedAt.Valid {
			o.CompletedAt = completedAt.Time
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// parseCryptoAmount 把存储的 crypto_amount 字符串（AssetAmount.HumanString）解析为 AssetAmount，
// 并按资产标准小数位归一化，确保锁与释放使用完全一致的最小单位值。
func parseCryptoAmount(s, asset string) settlement.AssetAmount {
	dec := settlement.AssetDecimalsByName(asset)
	aa, err := settlement.AssetAmountFromString(s, dec)
	if err != nil {
		return settlement.AssetAmount{Value: big.NewInt(0), Decimals: dec}
	}
	return aa.ToDecimals(dec)
}

func scanCounterparty(row *sql.Row) (*OtcCounterparty, error) {
	var cp OtcCounterparty
	err := row.Scan(&cp.ID, &cp.UserID, &cp.CounterpartyID, &cp.TradesTotal, &cp.TradesCompleted,
		&cp.RatingSum, &cp.RatingCount, &cp.CreatedAt, &cp.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrCounterpartyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &cp, nil
}

func scanCounterparties(rows *sql.Rows) ([]*OtcCounterparty, error) {
	out := make([]*OtcCounterparty, 0)
	for rows.Next() {
		var cp OtcCounterparty
		if err := rows.Scan(&cp.ID, &cp.UserID, &cp.CounterpartyID, &cp.TradesTotal, &cp.TradesCompleted,
			&cp.RatingSum, &cp.RatingCount, &cp.CreatedAt, &cp.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &cp)
	}
	return out, rows.Err()
}
