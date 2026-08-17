package options

import (
	"database/sql"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// mysqlStore 是 MySQL 版 Store。表名 ce_option_contracts / ce_option_positions 已遵守 ce_ 前缀约定。
type mysqlStore struct {
	db *sql.DB
}

func (s *mysqlStore) Close() error {
	return s.db.Close()
}

// --- 合约 ---

func (s *mysqlStore) CreateContract(c *OptionContract) error {
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	res, err := s.db.Exec(`
		INSERT INTO ce_option_contracts
			(underlying, quote_asset, strike, expiry, type, style, contract_size, premium, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Underlying, c.QuoteAsset, c.Strike, c.Expiry, string(c.Type), string(c.Style),
		c.ContractSize, c.Premium, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		c.ID = id
	}
	return nil
}

func (s *mysqlStore) GetContract(id int64) (*OptionContract, error) {
	row := s.db.QueryRow(`
		SELECT id, underlying, quote_asset, strike, expiry, type, style, contract_size, premium, created_at, updated_at
		FROM ce_option_contracts WHERE id = ?`, id)
	return scanContract(row)
}

func (s *mysqlStore) ListContracts() ([]*OptionContract, error) {
	rows, err := s.db.Query(`
		SELECT id, underlying, quote_asset, strike, expiry, type, style, contract_size, premium, created_at, updated_at
		FROM ce_option_contracts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanContracts(rows)
}

// --- 持仓 ---

func (s *mysqlStore) UpsertPosition(p *OptionPosition) error {
	now := time.Now()
	if p.OpenedAt.IsZero() {
		p.OpenedAt = now
	}
	p.UpdatedAt = now
	if p.ID == 0 {
		res, err := s.db.Exec(`
			INSERT INTO ce_option_positions
				(user_id, contract_id, side, quantity, quote_asset, premium, margin, status, opened_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.UserID, p.ContractID, string(p.Side), p.Quantity, p.QuoteAsset,
			p.Premium.HumanString(), p.Margin.HumanString(),
			string(p.Status), p.OpenedAt, p.UpdatedAt)
		if err != nil {
			return err
		}
		if id, err := res.LastInsertId(); err == nil {
			p.ID = id
		}
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO ce_option_positions
			(id, user_id, contract_id, side, quantity, quote_asset, premium, margin, status, opened_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			user_id=VALUES(user_id), contract_id=VALUES(contract_id), side=VALUES(side),
			quantity=VALUES(quantity), quote_asset=VALUES(quote_asset),
			premium=VALUES(premium), margin=VALUES(margin),
			status=VALUES(status), opened_at=VALUES(opened_at), updated_at=VALUES(updated_at)`,
		p.ID, p.UserID, p.ContractID, string(p.Side), p.Quantity, p.QuoteAsset,
		p.Premium.HumanString(), p.Margin.HumanString(),
		string(p.Status), p.OpenedAt, p.UpdatedAt)
	return err
}

func (s *mysqlStore) GetPosition(id int64) (*OptionPosition, error) {
	row := s.db.QueryRow(`
		SELECT id, user_id, contract_id, side, quantity, quote_asset, premium, margin, status, opened_at, updated_at
		FROM ce_option_positions WHERE id = ?`, id)
	return scanPosition(row)
}

func (s *mysqlStore) ListPositions(userID int64) ([]*OptionPosition, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, contract_id, side, quantity, quote_asset, premium, margin, status, opened_at, updated_at
		FROM ce_option_positions WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

func (s *mysqlStore) ListAllPositions() ([]*OptionPosition, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, contract_id, side, quantity, quote_asset, premium, margin, status, opened_at, updated_at
		FROM ce_option_positions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

func (s *mysqlStore) ListAllOpen() ([]*OptionPosition, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, contract_id, side, quantity, quote_asset, premium, margin, status, opened_at, updated_at
		FROM ce_option_positions WHERE status = ? ORDER BY id`, string(StatusOpen))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

func (s *mysqlStore) DeletePosition(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ce_option_positions WHERE id = ?`, id)
	return err
}

// --- 扫描辅助 ---

func scanContract(row *sql.Row) (*OptionContract, error) {
	var c OptionContract
	var typ, style string
	err := row.Scan(&c.ID, &c.Underlying, &c.QuoteAsset, &c.Strike, &c.Expiry,
		&typ, &style, &c.ContractSize, &c.Premium, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrContractNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Type = OptionType(typ)
	c.Style = ExerciseStyle(style)
	return &c, nil
}

func scanContracts(rows *sql.Rows) ([]*OptionContract, error) {
	out := make([]*OptionContract, 0)
	for rows.Next() {
		var c OptionContract
		var typ, style string
		if err := rows.Scan(&c.ID, &c.Underlying, &c.QuoteAsset, &c.Strike, &c.Expiry,
			&typ, &style, &c.ContractSize, &c.Premium, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Type = OptionType(typ)
		c.Style = ExerciseStyle(style)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// parseOptionAmount 把存储的 premium/margin 字符串（AssetAmount.HumanString）按持仓计价资产
// 的小数位解析为 AssetAmount。
func parseOptionAmount(s, quoteAsset string) settlement.AssetAmount {
	dec := settlement.AssetDecimalsByName(quoteAsset)
	aa, err := settlement.AssetAmountFromString(s, dec)
	if err != nil {
		return settlement.AssetAmount{Decimals: dec}
	}
	return aa.ToDecimals(dec)
}

func scanPosition(row *sql.Row) (*OptionPosition, error) {
	var p OptionPosition
	var side, status, quoteAsset, premiumStr, marginStr string
	err := row.Scan(&p.ID, &p.UserID, &p.ContractID, &side, &p.Quantity,
		&quoteAsset, &premiumStr, &marginStr, &status, &p.OpenedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrPositionNotFound
	}
	if err != nil {
		return nil, err
	}
	p.QuoteAsset = quoteAsset
	p.Premium = parseOptionAmount(premiumStr, quoteAsset)
	p.Margin = parseOptionAmount(marginStr, quoteAsset)
	p.Side = PositionSide(side)
	p.Status = PositionStatus(status)
	return &p, nil
}

func scanPositions(rows *sql.Rows) ([]*OptionPosition, error) {
	out := make([]*OptionPosition, 0)
	for rows.Next() {
		var p OptionPosition
		var side, status, quoteAsset, premiumStr, marginStr string
		if err := rows.Scan(&p.ID, &p.UserID, &p.ContractID, &side, &p.Quantity,
			&quoteAsset, &premiumStr, &marginStr, &status, &p.OpenedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.QuoteAsset = quoteAsset
		p.Premium = parseOptionAmount(premiumStr, quoteAsset)
		p.Margin = parseOptionAmount(marginStr, quoteAsset)
		p.Side = PositionSide(side)
		p.Status = PositionStatus(status)
		out = append(out, &p)
	}
	return out, rows.Err()
}
