package earn

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// mysqlStore 是 MySQL 版 Store。表名 ce_earn_* / ce_launch_* 已遵守 ce_ 前缀约定。
// 金额列以 VARCHAR(64) 存 AssetAmount.HumanString（最小单位十进制），避免 float 精度丢失
// （同 wealth 9703 迁移决策，F2）。
type mysqlStore struct {
	db *sql.DB
}

func (s *mysqlStore) Close() error { return s.db.Close() }

// nullableTime 零值时间转 SQL NULL。
func nullableTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}

// --- 理财产品 ---

const productCols = `id, name, asset, term_days, apy, min_amount, max_amount, status, created_at, updated_at`

func scanProduct(row *sql.Row) (*EarnProduct, error) {
	var p EarnProduct
	var created, updated time.Time
	if err := row.Scan(&p.ID, &p.Name, &p.Asset, &p.TermDays, &p.APY, &p.MinAmount, &p.MaxAmount,
		&p.Status, &created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = created, updated
	return &p, nil
}

func (s *mysqlStore) CreateProduct(p *EarnProduct) error {
	now := time.Now()
	p.CreatedAt, p.UpdatedAt = now, now
	res, err := s.db.Exec(`
		INSERT INTO ce_earn_products (name, asset, term_days, apy, min_amount, max_amount, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Asset, p.TermDays, p.APY, p.MinAmount, p.MaxAmount, string(p.Status), now, now)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		p.ID = id
	}
	return nil
}

func (s *mysqlStore) GetProduct(id int64) (*EarnProduct, error) {
	row := s.db.QueryRow(`SELECT `+productCols+` FROM ce_earn_products WHERE id = ?`, id)
	var p EarnProduct
	err := row.Scan(&p.ID, &p.Name, &p.Asset, &p.TermDays, &p.APY, &p.MinAmount, &p.MaxAmount,
		&p.Status, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrProductNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *mysqlStore) ListProducts(status ProductStatus) ([]*EarnProduct, error) {
	q := `SELECT ` + productCols + ` FROM ce_earn_products`
	args := []interface{}{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, string(status))
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EarnProduct
	for rows.Next() {
		var p EarnProduct
		if err := rows.Scan(&p.ID, &p.Name, &p.Asset, &p.TermDays, &p.APY, &p.MinAmount, &p.MaxAmount,
			&p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *mysqlStore) UpdateProduct(p *EarnProduct) error {
	p.UpdatedAt = time.Now()
	_, err := s.db.Exec(`
		UPDATE ce_earn_products SET name=?, asset=?, term_days=?, apy=?, min_amount=?, max_amount=?, status=?, updated_at=?
		WHERE id = ?`,
		p.Name, p.Asset, p.TermDays, p.APY, p.MinAmount, p.MaxAmount, string(p.Status), p.UpdatedAt, p.ID)
	return err
}

// --- 理财申购 ---

func (s *mysqlStore) CreateSubscription(x *EarnSubscription) error {
	now := time.Now()
	x.CreatedAt, x.LastAccrualAt = now, now
	res, err := s.db.Exec(`
		INSERT INTO ce_earn_subscriptions (user_id, product_id, asset, principal, accrued, status, created_at, last_accrual_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		x.UserID, x.ProductID, x.Asset, x.Principal.HumanString(), x.Accrued.HumanString(),
		string(x.Status), now, now)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		x.ID = id
	}
	return nil
}

func scanSubscription(row interface{ Scan(...interface{}) error }) (*EarnSubscription, error) {
	var x EarnSubscription
	var principal, accrued, redeemedAmt string
	var redeemed sql.NullTime
	if err := row.Scan(&x.ID, &x.UserID, &x.ProductID, &x.Asset, &principal, &accrued, &x.Status,
		&x.CreatedAt, &x.LastAccrualAt, &redeemed, &redeemedAmt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrProductNotFound
		}
		return nil, err
	}
	dec := settlement.AssetDecimalsByName(x.Asset)
	pv, _ := settlement.AssetAmountFromString(principal, dec)
	av, _ := settlement.AssetAmountFromString(accrued, dec)
	rv, _ := settlement.AssetAmountFromString(redeemedAmt, dec)
	x.Principal, x.Accrued, x.RedeemedAmount = pv, av, rv
	if redeemed.Valid {
		x.RedeemedAt = redeemed.Time
	}
	return &x, nil
}

func (s *mysqlStore) GetSubscription(id int64) (*EarnSubscription, error) {
	row := s.db.QueryRow(`
		SELECT id, user_id, product_id, asset, principal, accrued, status, created_at, last_accrual_at, redeemed_at, redeemed_amount
		FROM ce_earn_subscriptions WHERE id = ?`, id)
	return scanSubscription(row)
}

func (s *mysqlStore) UpdateSubscription(x *EarnSubscription) error {
	_, err := s.db.Exec(`
		UPDATE ce_earn_subscriptions SET principal=?, accrued=?, status=?, last_accrual_at=?, redeemed_at=?, redeemed_amount=?
		WHERE id = ?`,
		x.Principal.HumanString(), x.Accrued.HumanString(), string(x.Status), x.LastAccrualAt,
		nullableTime(x.RedeemedAt), x.RedeemedAmount.HumanString(), x.ID)
	return err
}

func (s *mysqlStore) DeleteSubscription(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ce_earn_subscriptions WHERE id = ?`, id)
	return err
}

func (s *mysqlStore) listSubscriptions(q string, args ...interface{}) ([]*EarnSubscription, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EarnSubscription
	for rows.Next() {
		x, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *mysqlStore) ListSubscriptions(userID int64) ([]*EarnSubscription, error) {
	return s.listSubscriptions(`
		SELECT id, user_id, product_id, asset, principal, accrued, status, created_at, last_accrual_at, redeemed_at, redeemed_amount
		FROM ce_earn_subscriptions WHERE user_id = ? ORDER BY id DESC`, userID)
}

func (s *mysqlStore) ListAllSubscriptions() ([]*EarnSubscription, error) {
	return s.listSubscriptions(`
		SELECT id, user_id, product_id, asset, principal, accrued, status, created_at, last_accrual_at, redeemed_at, redeemed_amount
		FROM ce_earn_subscriptions`)
}

// --- Launchpool 项目 ---

func (s *mysqlStore) CreateProject(p *LaunchProject) error {
	poolsJSON, err := json.Marshal(p.Pools)
	if err != nil {
		return err
	}
	now := time.Now()
	p.CreatedAt = now
	res, err := s.db.Exec(`
		INSERT INTO ce_launch_projects (name, token, total_supply, starts_at, ends_at, pools_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Token, p.TotalSupply, p.StartsAt, p.EndsAt, string(poolsJSON), now)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		p.ID = id
	}
	return nil
}

const projectCols = `id, name, token, total_supply, starts_at, ends_at, pools_json, funded_total`

func scanProject(row interface{ Scan(...interface{}) error }) (*LaunchProject, error) {
	var p LaunchProject
	var poolsJSON, fundedTotal string
	if err := row.Scan(&p.ID, &p.Name, &p.Token, &p.TotalSupply, &p.StartsAt, &p.EndsAt, &poolsJSON, &fundedTotal); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrProjectNotFound
		}
		return nil, err
	}
	if poolsJSON != "" {
		_ = json.Unmarshal([]byte(poolsJSON), &p.Pools)
	}
	p.FundedTotal, _ = settlement.AssetAmountFromString(fundedTotal, settlement.AssetDecimalsByName(p.Token))
	return &p, nil
}

func (s *mysqlStore) GetProject(id int64) (*LaunchProject, error) {
	row := s.db.QueryRow(`SELECT `+projectCols+` FROM ce_launch_projects WHERE id = ?`, id)
	return scanProject(row)
}

func (s *mysqlStore) AddProjectFunded(id int64, d settlement.AssetAmount) error {
	cur, err := s.GetProject(id)
	if err != nil {
		return err
	}
	next := cur.FundedTotal.Add(d)
	_, err = s.db.Exec(`UPDATE ce_launch_projects SET funded_total=? WHERE id=?`, next.HumanString(), id)
	return err
}

func (s *mysqlStore) ListProjects() ([]*LaunchProject, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM ce_launch_projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LaunchProject
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- Launchpool 仓位 ---

const positionCols = `id, user_id, project_id, pool_id, asset, token, staked, rewards_pending, harvested_total,
	status, stake_seq, unstake_seq, harvest_seq, created_at, last_accrual_at`

func scanPosition(row interface{ Scan(...interface{}) error }) (*LaunchPosition, error) {
	var pos LaunchPosition
	var staked, pending, harvested string
	if err := row.Scan(&pos.ID, &pos.UserID, &pos.ProjectID, &pos.PoolID, &pos.Asset, &pos.Token,
		&staked, &pending, &harvested, &pos.Status, &pos.StakeSeq, &pos.UnstakeSeq, &pos.HarvestSeq,
		&pos.CreatedAt, &pos.LastAccrualAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrPositionNotFound
		}
		return nil, err
	}
	stakedAmt, _ := settlement.AssetAmountFromString(staked, settlement.AssetDecimalsByName(pos.Asset))
	tokDec := settlement.AssetDecimalsByName(pos.Token)
	pendingAmt, _ := settlement.AssetAmountFromString(pending, tokDec)
	harvestedAmt, _ := settlement.AssetAmountFromString(harvested, tokDec)
	pos.Staked, pos.RewardsPending, pos.HarvestedTotal = stakedAmt, pendingAmt, harvestedAmt
	return &pos, nil
}

func (s *mysqlStore) UpsertPosition(pos *LaunchPosition) error {
	now := time.Now()
	if pos.CreatedAt.IsZero() {
		pos.CreatedAt = now
	}
	if pos.ID > 0 {
		_, err := s.db.Exec(`
			UPDATE ce_launch_positions SET
				staked=?, rewards_pending=?, harvested_total=?, status=?,
				stake_seq=?, unstake_seq=?, harvest_seq=?, last_accrual_at=?
			WHERE id = ?`,
			pos.Staked.HumanString(), pos.RewardsPending.HumanString(), pos.HarvestedTotal.HumanString(),
			string(pos.Status), pos.StakeSeq, pos.UnstakeSeq, pos.HarvestSeq, pos.LastAccrualAt, pos.ID)
		return err
	}
	res, err := s.db.Exec(`
		INSERT INTO ce_launch_positions
			(user_id, project_id, pool_id, asset, token, staked, rewards_pending, harvested_total, status,
			 stake_seq, unstake_seq, harvest_seq, created_at, last_accrual_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		pos.UserID, pos.ProjectID, strings.ToLower(pos.PoolID), pos.Asset, pos.Token,
		pos.Staked.HumanString(), pos.RewardsPending.HumanString(), pos.HarvestedTotal.HumanString(),
		string(pos.Status), pos.StakeSeq, now, pos.LastAccrualAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		pos.ID = id
	}
	return nil
}

func (s *mysqlStore) DeletePosition(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ce_launch_positions WHERE id = ?`, id)
	return err
}

func (s *mysqlStore) GetPosition(id int64) (*LaunchPosition, error) {
	row := s.db.QueryRow(`SELECT `+positionCols+` FROM ce_launch_positions WHERE id = ?`, id)
	return scanPosition(row)
}

func (s *mysqlStore) FindPosition(userID, projectID int64, poolID string) (*LaunchPosition, error) {
	row := s.db.QueryRow(`SELECT `+positionCols+` FROM ce_launch_positions WHERE user_id=? AND project_id=? AND pool_id=?`,
		userID, projectID, strings.ToLower(poolID))
	return scanPosition(row)
}

func (s *mysqlStore) listPositions(q string, args ...interface{}) ([]*LaunchPosition, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LaunchPosition
	for rows.Next() {
		pos, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pos)
	}
	return out, rows.Err()
}

func (s *mysqlStore) ListPositions(userID int64) ([]*LaunchPosition, error) {
	return s.listPositions(`SELECT `+positionCols+` FROM ce_launch_positions WHERE user_id=? ORDER BY id`, userID)
}

func (s *mysqlStore) ListAllPositions() ([]*LaunchPosition, error) {
	return s.listPositions(`SELECT ` + positionCols + ` FROM ce_launch_positions`)
}
