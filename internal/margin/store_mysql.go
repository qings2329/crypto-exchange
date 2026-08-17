package margin

import (
	"database/sql"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// mysqlStore 是 MySQL 版 Store。表名 ce_margin_accounts 已遵守 ce_ 前缀约定。
type mysqlStore struct {
	db *sql.DB
}

func (s *mysqlStore) Close() error {
	return s.db.Close()
}

func (s *mysqlStore) UpsertAccount(a *MarginAccount) error {
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	_, err := s.db.Exec(`
		INSERT INTO ce_margin_accounts
			(user_id, asset, collateral_asset, collateral_amount, debt, interest_accrued,
			 leverage, status, last_accrual, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			collateral_asset = VALUES(collateral_asset),
			collateral_amount = VALUES(collateral_amount),
			debt = VALUES(debt),
			interest_accrued = VALUES(interest_accrued),
			leverage = VALUES(leverage),
			status = VALUES(status),
			last_accrual = VALUES(last_accrual),
			updated_at = VALUES(updated_at)`,
		a.UserID, a.Asset, a.CollateralAsset, a.CollateralAmount.HumanString(), a.Debt.HumanString(),
		a.InterestAccrued.HumanString(), a.Leverage, string(a.Status), a.LastAccrual, a.CreatedAt, a.UpdatedAt,
	)
	return err
}

func (s *mysqlStore) GetAccount(userID int64, asset string) (*MarginAccount, error) {
	row := s.db.QueryRow(`
		SELECT user_id, asset, collateral_asset, collateral_amount, debt, interest_accrued,
		       leverage, status, last_accrual, created_at, updated_at
		FROM ce_margin_accounts WHERE user_id = ? AND asset = ?`, userID, asset)
	return scanAccount(row)
}

func (s *mysqlStore) ListAccounts(userID int64) ([]*MarginAccount, error) {
	rows, err := s.db.Query(`
		SELECT user_id, asset, collateral_asset, collateral_amount, debt, interest_accrued,
		       leverage, status, last_accrual, created_at, updated_at
		FROM ce_margin_accounts WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccounts(rows)
}

func (s *mysqlStore) ListAllActive() ([]*MarginAccount, error) {
	rows, err := s.db.Query(`
		SELECT user_id, asset, collateral_asset, collateral_amount, debt, interest_accrued,
		       leverage, status, last_accrual, created_at, updated_at
		FROM ce_margin_accounts WHERE status = ?`, string(StatusActive))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccounts(rows)
}

func (s *mysqlStore) DeleteAccount(userID int64, asset string) error {
	_, err := s.db.Exec(`DELETE FROM ce_margin_accounts WHERE user_id = ? AND asset = ?`, userID, asset)
	return err
}

// parseMarginAmount 把存储的抵押/债务/利息字符串（AssetAmount.HumanString）按对应资产的小数位
// 解析为 AssetAmount：collateral 按 collateral_asset，debt/interest 按 asset。
func parseMarginAmount(s, asset string) settlement.AssetAmount {
	dec := settlement.AssetDecimalsByName(asset)
	aa, err := settlement.AssetAmountFromString(s, dec)
	if err != nil {
		return settlement.AssetAmount{Decimals: dec}
	}
	return aa.ToDecimals(dec)
}

func scanAccount(row *sql.Row) (*MarginAccount, error) {
	var a MarginAccount
	var status, collateralStr, debtStr, interestStr string
	err := row.Scan(&a.UserID, &a.Asset, &a.CollateralAsset, &collateralStr, &debtStr,
		&interestStr, &a.Leverage, &status, &a.LastAccrual, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CollateralAmount = parseMarginAmount(collateralStr, a.CollateralAsset)
	a.Debt = parseMarginAmount(debtStr, a.Asset)
	a.InterestAccrued = parseMarginAmount(interestStr, a.Asset)
	a.Status = AccountStatus(status)
	return &a, nil
}

func scanAccounts(rows *sql.Rows) ([]*MarginAccount, error) {
	out := make([]*MarginAccount, 0)
	for rows.Next() {
		var a MarginAccount
		var status, collateralStr, debtStr, interestStr string
		if err := rows.Scan(&a.UserID, &a.Asset, &a.CollateralAsset, &collateralStr, &debtStr,
			&interestStr, &a.Leverage, &status, &a.LastAccrual, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.CollateralAmount = parseMarginAmount(collateralStr, a.CollateralAsset)
		a.Debt = parseMarginAmount(debtStr, a.Asset)
		a.InterestAccrued = parseMarginAmount(interestStr, a.Asset)
		a.Status = AccountStatus(status)
		out = append(out, &a)
	}
	return out, rows.Err()
}
