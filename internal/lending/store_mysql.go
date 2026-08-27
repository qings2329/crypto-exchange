package lending

import (
	"database/sql"
	"encoding/json"
	"math/big"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// mysqlStore 是借贷业务的 MySQL 实现。表名 ce_lending_pools / ce_lend_orders /
// ce_borrow_orders / ce_lending_interest 遵守 ce_ 前缀约定；金额以 JSON 字符串
// （HumanString 格式）自包含定点存储。
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

// ---- 资金池 ----

func (s *mysqlStore) CreatePool(p *LendingPool) error {
	supplyJSON := mustMarshalAsset(p.TotalSupply)
	borrowJSON := mustMarshalAsset(p.TotalBorrow)
	availJSON := mustMarshalAsset(p.Available)
	res, err := s.db.Exec(`INSERT INTO ce_lending_pools
		(asset, total_supply_json, total_borrow_json, available_json, utilization,
		 interest_rate, collateral_req, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Asset, supplyJSON, borrowJSON, availJSON, p.Utilization,
		p.InterestRate, p.CollateralReq, string(p.Status), p.CreatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		p.ID = id
	}
	return nil
}

func (s *mysqlStore) GetPool(id int64) (*LendingPool, error) {
	row := s.db.QueryRow(`SELECT id, asset, total_supply_json, total_borrow_json, available_json,
		utilization, interest_rate, collateral_req, status, created_at
		FROM ce_lending_pools WHERE id = ?`, id)
	return scanPool(row)
}

func (s *mysqlStore) GetPoolByAsset(asset string) (*LendingPool, error) {
	row := s.db.QueryRow(`SELECT id, asset, total_supply_json, total_borrow_json, available_json,
		utilization, interest_rate, collateral_req, status, created_at
		FROM ce_lending_pools WHERE asset = ?`, asset)
	return scanPool(row)
}

func (s *mysqlStore) ListPools(status PoolStatus) ([]*LendingPool, error) {
	q := `SELECT id, asset, total_supply_json, total_borrow_json, available_json,
		utilization, interest_rate, collateral_req, status, created_at
		FROM ce_lending_pools`
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
	return scanPools(rows)
}

func (s *mysqlStore) UpdatePool(p *LendingPool) error {
	supplyJSON := mustMarshalAsset(p.TotalSupply)
	borrowJSON := mustMarshalAsset(p.TotalBorrow)
	availJSON := mustMarshalAsset(p.Available)
	_, err := s.db.Exec(`UPDATE ce_lending_pools SET
		total_supply_json=?, total_borrow_json=?, available_json=?, utilization=?,
		interest_rate=?, collateral_req=?, status=?
		WHERE id = ?`,
		supplyJSON, borrowJSON, availJSON, p.Utilization,
		p.InterestRate, p.CollateralReq, string(p.Status), p.ID)
	return err
}

// ---- 存款订单 ----

func (s *mysqlStore) CreateLendOrder(o *LendOrder) error {
	amountJSON := mustMarshalAsset(o.Amount)
	res, err := s.db.Exec(`INSERT INTO ce_lend_orders
		(user_id, pool_id, amount_json, rate, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		o.UserID, o.PoolID, amountJSON, o.Rate, o.Status, o.CreatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		o.ID = id
	}
	return nil
}

func (s *mysqlStore) GetLendOrder(id int64) (*LendOrder, error) {
	row := s.db.QueryRow(`SELECT id, user_id, pool_id, amount_json, rate, status, created_at
		FROM ce_lend_orders WHERE id = ?`, id)
	return scanLendOrder(row)
}

func (s *mysqlStore) ListLendOrdersByUser(uid int64) ([]*LendOrder, error) {
	rows, err := s.db.Query(`SELECT id, user_id, pool_id, amount_json, rate, status, created_at
		FROM ce_lend_orders WHERE user_id = ? ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLendOrders(rows)
}

func (s *mysqlStore) ListLendOrdersByPool(pid int64) ([]*LendOrder, error) {
	rows, err := s.db.Query(`SELECT id, user_id, pool_id, amount_json, rate, status, created_at
		FROM ce_lend_orders WHERE pool_id = ? AND status = 'active' ORDER BY id`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLendOrders(rows)
}

func (s *mysqlStore) ListAllLendOrders() ([]*LendOrder, error) {
	rows, err := s.db.Query(`SELECT id, user_id, pool_id, amount_json, rate, status, created_at
		FROM ce_lend_orders ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLendOrders(rows)
}

func (s *mysqlStore) UpdateLendOrder(o *LendOrder) error {
	_, err := s.db.Exec(`UPDATE ce_lend_orders SET status = ? WHERE id = ?`,
		o.Status, o.ID)
	return err
}

// ---- 借款订单 ----

func (s *mysqlStore) CreateBorrowOrder(o *BorrowOrder) error {
	amountJSON := mustMarshalAsset(o.Amount)
	collJSON := mustMarshalAsset(o.Collateral)
	interestJSON := mustMarshalAsset(o.InterestAcc)
	res, err := s.db.Exec(`INSERT INTO ce_borrow_orders
		(user_id, pool_id, amount_json, collateral_json, rate, interest_acc_json, status, created_at, repaid_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.UserID, o.PoolID, amountJSON, collJSON, o.Rate, interestJSON, o.Status, o.CreatedAt, o.RepaidAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		o.ID = id
	}
	return nil
}

func (s *mysqlStore) GetBorrowOrder(id int64) (*BorrowOrder, error) {
	row := s.db.QueryRow(`SELECT id, user_id, pool_id, amount_json, collateral_json, rate,
		interest_acc_json, status, created_at, repaid_at
		FROM ce_borrow_orders WHERE id = ?`, id)
	return scanBorrowOrder(row)
}

func (s *mysqlStore) ListBorrowOrdersByUser(uid int64) ([]*BorrowOrder, error) {
	rows, err := s.db.Query(`SELECT id, user_id, pool_id, amount_json, collateral_json, rate,
		interest_acc_json, status, created_at, repaid_at
		FROM ce_borrow_orders WHERE user_id = ? ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBorrowOrders(rows)
}

func (s *mysqlStore) ListBorrowOrdersByPool(pid int64) ([]*BorrowOrder, error) {
	rows, err := s.db.Query(`SELECT id, user_id, pool_id, amount_json, collateral_json, rate,
		interest_acc_json, status, created_at, repaid_at
		FROM ce_borrow_orders WHERE pool_id = ? ORDER BY id`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBorrowOrders(rows)
}

func (s *mysqlStore) ListAllBorrowOrders() ([]*BorrowOrder, error) {
	rows, err := s.db.Query(`SELECT id, user_id, pool_id, amount_json, collateral_json, rate,
		interest_acc_json, status, created_at, repaid_at
		FROM ce_borrow_orders ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBorrowOrders(rows)
}

func (s *mysqlStore) ListActiveBorrowOrders() ([]*BorrowOrder, error) {
	rows, err := s.db.Query(`SELECT id, user_id, pool_id, amount_json, collateral_json, rate,
		interest_acc_json, status, created_at, repaid_at
		FROM ce_borrow_orders WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBorrowOrders(rows)
}

func (s *mysqlStore) UpdateBorrowOrder(o *BorrowOrder) error {
	interestJSON := mustMarshalAsset(o.InterestAcc)
	_, err := s.db.Exec(`UPDATE ce_borrow_orders SET
		status=?, interest_acc_json=?, repaid_at=?
		WHERE id = ?`,
		o.Status, interestJSON, o.RepaidAt, o.ID)
	return err
}

// ---- 利息记录 ----

func (s *mysqlStore) CreateInterestRecord(r *InterestRecord) error {
	amountJSON := mustMarshalAsset(r.Amount)
	res, err := s.db.Exec(`INSERT INTO ce_lending_interest
		(pool_id, user_id, type, amount_json, recorded_at)
		VALUES (?, ?, ?, ?, ?)`,
		r.PoolID, r.UserID, r.Type, amountJSON, r.RecordedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		r.ID = id
	}
	return nil
}

func (s *mysqlStore) ListInterestRecordsByUser(uid int64) ([]*InterestRecord, error) {
	rows, err := s.db.Query(`SELECT id, pool_id, user_id, type, amount_json, recorded_at
		FROM ce_lending_interest WHERE user_id = ? ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInterestRecords(rows)
}

// ---- 序列化辅助 ----

func mustMarshalAsset(a settlement.AssetAmount) string {
	b, _ := json.Marshal(a)
	return string(b)
}

func unmarshalAsset(s string) settlement.AssetAmount {
	var a settlement.AssetAmount
	if s == "" || s == "{}" {
		return settlement.NewAssetAmount(big.NewInt(0), 8)
	}
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return settlement.NewAssetAmount(big.NewInt(0), 8)
	}
	return a
}

// ---- 扫描辅助 ----

func scanPool(row *sql.Row) (*LendingPool, error) {
	var p LendingPool
	var status string
	var supplyJSON, borrowJSON, availJSON string
	err := row.Scan(&p.ID, &p.Asset, &supplyJSON, &borrowJSON, &availJSON,
		&p.Utilization, &p.InterestRate, &p.CollateralReq, &status, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrPoolNotFound
	}
	if err != nil {
		return nil, err
	}
	p.TotalSupply = unmarshalAsset(supplyJSON)
	p.TotalBorrow = unmarshalAsset(borrowJSON)
	p.Available = unmarshalAsset(availJSON)
	p.Status = PoolStatus(status)
	return &p, nil
}

func scanPools(rows *sql.Rows) ([]*LendingPool, error) {
	out := make([]*LendingPool, 0)
	for rows.Next() {
		var p LendingPool
		var status string
		var supplyJSON, borrowJSON, availJSON string
		if err := rows.Scan(&p.ID, &p.Asset, &supplyJSON, &borrowJSON, &availJSON,
			&p.Utilization, &p.InterestRate, &p.CollateralReq, &status, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.TotalSupply = unmarshalAsset(supplyJSON)
		p.TotalBorrow = unmarshalAsset(borrowJSON)
		p.Available = unmarshalAsset(availJSON)
		p.Status = PoolStatus(status)
		out = append(out, &p)
	}
	return out, rows.Err()
}

func scanLendOrder(row *sql.Row) (*LendOrder, error) {
	var o LendOrder
	var amountJSON string
	err := row.Scan(&o.ID, &o.UserID, &o.PoolID, &amountJSON, &o.Rate, &o.Status, &o.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	o.Amount = unmarshalAsset(amountJSON)
	return &o, nil
}

func scanLendOrders(rows *sql.Rows) ([]*LendOrder, error) {
	out := make([]*LendOrder, 0)
	for rows.Next() {
		var o LendOrder
		var amountJSON string
		if err := rows.Scan(&o.ID, &o.UserID, &o.PoolID, &amountJSON, &o.Rate, &o.Status, &o.CreatedAt); err != nil {
			return nil, err
		}
		o.Amount = unmarshalAsset(amountJSON)
		out = append(out, &o)
	}
	return out, rows.Err()
}

func scanBorrowOrder(row *sql.Row) (*BorrowOrder, error) {
	var o BorrowOrder
	var amountJSON, collJSON, interestJSON string
	err := row.Scan(&o.ID, &o.UserID, &o.PoolID, &amountJSON, &collJSON, &o.Rate,
		&interestJSON, &o.Status, &o.CreatedAt, &o.RepaidAt)
	if err == sql.ErrNoRows {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	o.Amount = unmarshalAsset(amountJSON)
	o.Collateral = unmarshalAsset(collJSON)
	o.InterestAcc = unmarshalAsset(interestJSON)
	return &o, nil
}

func scanBorrowOrders(rows *sql.Rows) ([]*BorrowOrder, error) {
	out := make([]*BorrowOrder, 0)
	for rows.Next() {
		var o BorrowOrder
		var amountJSON, collJSON, interestJSON string
		if err := rows.Scan(&o.ID, &o.UserID, &o.PoolID, &amountJSON, &collJSON, &o.Rate,
			&interestJSON, &o.Status, &o.CreatedAt, &o.RepaidAt); err != nil {
			return nil, err
		}
		o.Amount = unmarshalAsset(amountJSON)
		o.Collateral = unmarshalAsset(collJSON)
		o.InterestAcc = unmarshalAsset(interestJSON)
		out = append(out, &o)
	}
	return out, rows.Err()
}

func scanInterestRecords(rows *sql.Rows) ([]*InterestRecord, error) {
	out := make([]*InterestRecord, 0)
	for rows.Next() {
		var r InterestRecord
		var amountJSON string
		if err := rows.Scan(&r.ID, &r.PoolID, &r.UserID, &r.Type, &amountJSON, &r.RecordedAt); err != nil {
			return nil, err
		}
		r.Amount = unmarshalAsset(amountJSON)
		out = append(out, &r)
	}
	return out, rows.Err()
}
