package staking

import (
	"database/sql"
	"math/big"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// mysqlStore 是质押业务的 MySQL 实现。表名 ce_staking_products / ce_staking_delegations /
// ce_staking_rewards 遵守 ce_ 前缀约定；金额以「BIGINT value + INT decimals」自包含定点存储。
type mysqlStore struct {
	db *sql.DB
}

// NewMySQLStore 打开 MySQL 并跑迁移（建表），失败返回错误。
func NewMySQLStore(dsn string) (*mysqlStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate.New(db, Migrations).Up(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &mysqlStore{db: db}, nil
}

func (s *mysqlStore) Close() error { return s.db.Close() }

// ---- 产品 ----

func (s *mysqlStore) CreateProduct(p *StakingProduct) error {
	now := time.Now().Unix()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	res, err := s.db.Exec(`INSERT INTO ce_staking_products
		(name, chain, validator, contract_addr, asset, annual_rate, duration_days, min_amount, min_amount_decimals, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Chain, p.Validator, p.ContractAddr, p.Asset, p.AnnualRate, p.DurationDays,
		p.MinAmount.Value.Int64(), p.MinAmount.Decimals, string(p.Status), p.CreatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		p.ID = id
	}
	return nil
}

func (s *mysqlStore) GetProduct(id int64) (*StakingProduct, error) {
	row := s.db.QueryRow(`SELECT id, name, chain, validator, contract_addr, asset, annual_rate,
		duration_days, min_amount, min_amount_decimals, status, created_at
		FROM ce_staking_products WHERE id = ?`, id)
	return scanProduct(row)
}

func (s *mysqlStore) ListProducts(status ProductStatus) ([]*StakingProduct, error) {
	q := `SELECT id, name, chain, validator, contract_addr, asset, annual_rate,
		duration_days, min_amount, min_amount_decimals, status, created_at FROM ce_staking_products`
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

func (s *mysqlStore) CloseProduct(id int64) error {
	_, err := s.db.Exec(`UPDATE ce_staking_products SET status = ? WHERE id = ?`,
		string(ProductClosed), id)
	return err
}

// ---- 委托 ----

func (s *mysqlStore) CreateDelegation(d *StakingDelegation) error {
	now := time.Now().Unix()
	if d.CreatedAt == 0 {
		d.CreatedAt = now
	}
	res, err := s.db.Exec(`INSERT INTO ce_staking_delegations
		(user_id, product_id, principal, principal_decimals, status, tx_hash, created_at, unbond_at, unbonded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.UserID, d.ProductID, d.Principal.Value.Int64(), d.Principal.Decimals,
		string(d.Status), d.TxHash, d.CreatedAt, d.UnbondAt, d.UnbondedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		d.ID = id
	}
	return nil
}

func (s *mysqlStore) GetDelegation(id int64) (*StakingDelegation, error) {
	row := s.db.QueryRow(`SELECT id, user_id, product_id, principal, principal_decimals, status,
		tx_hash, created_at, unbond_at, unbonded_at FROM ce_staking_delegations WHERE id = ?`, id)
	return scanDelegation(row)
}

func (s *mysqlStore) ListDelegationsByUser(uid int64) ([]*StakingDelegation, error) {
	rows, err := s.db.Query(`SELECT id, user_id, product_id, principal, principal_decimals, status,
		tx_hash, created_at, unbond_at, unbonded_at FROM ce_staking_delegations
		WHERE user_id = ? ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDelegations(rows)
}

func (s *mysqlStore) ListAllDelegations() ([]*StakingDelegation, error) {
	rows, err := s.db.Query(`SELECT id, user_id, product_id, principal, principal_decimals, status,
		tx_hash, created_at, unbond_at, unbonded_at FROM ce_staking_delegations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDelegations(rows)
}

func (s *mysqlStore) UpdateDelegation(d *StakingDelegation) error {
	_, err := s.db.Exec(`UPDATE ce_staking_delegations SET
		principal=?, principal_decimals=?, status=?, tx_hash=?, unbond_at=?, unbonded_at=?
		WHERE id = ?`,
		d.Principal.Value.Int64(), d.Principal.Decimals, string(d.Status), d.TxHash,
		d.UnbondAt, d.UnbondedAt, d.ID)
	return err
}

func (s *mysqlStore) DeleteDelegation(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ce_staking_delegations WHERE id = ?`, id)
	return err
}

// ---- 奖励 ----

func (s *mysqlStore) CreateReward(r *StakingReward) error {
	res, err := s.db.Exec(`INSERT INTO ce_staking_rewards
		(delegation_id, amount, amount_decimals, accrued_at) VALUES (?, ?, ?, ?)`,
		r.DelegationID, r.Amount.Value.Int64(), r.Amount.Decimals, r.AccruedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		r.ID = id
	}
	return nil
}

func (s *mysqlStore) ListRewardsByDelegation(did int64) ([]*StakingReward, error) {
	rows, err := s.db.Query(`SELECT id, delegation_id, amount, amount_decimals, accrued_at
		FROM ce_staking_rewards WHERE delegation_id = ? ORDER BY id`, did)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRewards(rows)
}

// ---- 扫描辅助 ----

func scanProduct(row *sql.Row) (*StakingProduct, error) {
	var p StakingProduct
	var status string
	var minVal int64
	var minDec int
	err := row.Scan(&p.ID, &p.Name, &p.Chain, &p.Validator, &p.ContractAddr, &p.Asset,
		&p.AnnualRate, &p.DurationDays, &minVal, &minDec, &status, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrProductNotFound
	}
	if err != nil {
		return nil, err
	}
	p.MinAmount = settlement.NewAssetAmount(big.NewInt(minVal), minDec)
	p.Status = ProductStatus(status)
	return &p, nil
}

func scanProducts(rows *sql.Rows) ([]*StakingProduct, error) {
	out := make([]*StakingProduct, 0)
	for rows.Next() {
		var p StakingProduct
		var status string
		var minVal int64
		var minDec int
		if err := rows.Scan(&p.ID, &p.Name, &p.Chain, &p.Validator, &p.ContractAddr, &p.Asset,
			&p.AnnualRate, &p.DurationDays, &minVal, &minDec, &status, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.MinAmount = settlement.NewAssetAmount(big.NewInt(minVal), minDec)
		p.Status = ProductStatus(status)
		out = append(out, &p)
	}
	return out, rows.Err()
}

func scanDelegation(row *sql.Row) (*StakingDelegation, error) {
	var d StakingDelegation
	var status, txHash string
	var pVal int64
	var pDec int
	err := row.Scan(&d.ID, &d.UserID, &d.ProductID, &pVal, &pDec, &status, &txHash,
		&d.CreatedAt, &d.UnbondAt, &d.UnbondedAt)
	if err == sql.ErrNoRows {
		return nil, ErrDelegationNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Principal = settlement.NewAssetAmount(big.NewInt(pVal), pDec)
	d.Status = DelegationStatus(status)
	d.TxHash = txHash
	return &d, nil
}

func scanDelegations(rows *sql.Rows) ([]*StakingDelegation, error) {
	out := make([]*StakingDelegation, 0)
	for rows.Next() {
		var d StakingDelegation
		var status, txHash string
		var pVal int64
		var pDec int
		if err := rows.Scan(&d.ID, &d.UserID, &d.ProductID, &pVal, &pDec, &status, &txHash,
			&d.CreatedAt, &d.UnbondAt, &d.UnbondedAt); err != nil {
			return nil, err
		}
		d.Principal = settlement.NewAssetAmount(big.NewInt(pVal), pDec)
		d.Status = DelegationStatus(status)
		d.TxHash = txHash
		out = append(out, &d)
	}
	return out, rows.Err()
}

func scanRewards(rows *sql.Rows) ([]*StakingReward, error) {
	out := make([]*StakingReward, 0)
	for rows.Next() {
		var r StakingReward
		var aVal int64
		var aDec int
		if err := rows.Scan(&r.ID, &r.DelegationID, &aVal, &aDec, &r.AccruedAt); err != nil {
			return nil, err
		}
		r.Amount = settlement.NewAssetAmount(big.NewInt(aVal), aDec)
		out = append(out, &r)
	}
	return out, rows.Err()
}
