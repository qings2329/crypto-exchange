package bot

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// mysqlStore 是交易机器人业务的 MySQL 实现。表名 ce_bot_strategies / ce_bot_orders 遵守 ce_ 前缀约定；
// 策略参数（BotParams）以 JSON 列自包含存储，订单价格/数量使用 DOUBLE（下单记录，不影响账本资金安全——
// 真实资金在 spot/futures 后端以 settlement.AssetAmount 定点处理）。
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

// ---- 策略 ----

func (s *mysqlStore) CreateStrategy(st *BotStrategy) error {
	now := time.Now().Unix()
	if st.CreatedAt == 0 {
		st.CreatedAt = now
	}
	params, err := json.Marshal(st.Params)
	if err != nil {
		return err
	}
	var gridStateJSON []byte
	if st.GridState != nil {
		gridStateJSON, _ = json.Marshal(st.GridState)
	}
	res, err := s.db.Exec(`INSERT INTO ce_bot_strategies
		(user_id, name, market, symbol, side, type, params, status, grid_state, user_token, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		st.UserID, st.Name, string(st.Market), st.Symbol, st.Side, string(st.Type),
		string(params), string(st.Status), gridStateJSON, st.UserToken, st.CreatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		st.ID = id
	}
	return nil
}

func (s *mysqlStore) GetStrategy(id int64) (*BotStrategy, error) {
	row := s.db.QueryRow(`SELECT id, user_id, name, market, symbol, side, type, params, status, grid_state, user_token, created_at
		FROM ce_bot_strategies WHERE id = ?`, id)
	return scanStrategy(row)
}

func (s *mysqlStore) ListStrategiesByUser(uid int64) ([]*BotStrategy, error) {
	rows, err := s.db.Query(`SELECT id, user_id, name, market, symbol, side, type, params, status, grid_state, user_token, created_at
		FROM ce_bot_strategies WHERE user_id = ? ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrategies(rows)
}

func (s *mysqlStore) ListActiveStrategies() ([]*BotStrategy, error) {
	rows, err := s.db.Query(`SELECT id, user_id, name, market, symbol, side, type, params, status, grid_state, user_token, created_at
		FROM ce_bot_strategies WHERE status = ? ORDER BY id`, string(StrategyActive))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrategies(rows)
}

func (s *mysqlStore) ListAllStrategies() ([]*BotStrategy, error) {
	rows, err := s.db.Query(`SELECT id, user_id, name, market, symbol, side, type, params, status, grid_state, user_token, created_at
		FROM ce_bot_strategies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrategies(rows)
}

func (s *mysqlStore) UpdateStrategy(st *BotStrategy) error {
	params, err := json.Marshal(st.Params)
	if err != nil {
		return err
	}
	var gridStateJSON []byte
	if st.GridState != nil {
		gridStateJSON, _ = json.Marshal(st.GridState)
	}
	_, err = s.db.Exec(`UPDATE ce_bot_strategies SET
		name=?, market=?, symbol=?, side=?, type=?, params=?, status=?, grid_state=?, user_token=?
		WHERE id = ?`,
		st.Name, string(st.Market), st.Symbol, st.Side, string(st.Type), string(params),
		string(st.Status), gridStateJSON, st.UserToken, st.ID)
	return err
}

// ---- 订单 ----

func (s *mysqlStore) CreateOrder(o *BotOrder) error {
	res, err := s.db.Exec(`INSERT INTO ce_bot_orders
		(strategy_id, user_id, market, symbol, side, price, qty, client_oid, exchange_order_id, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.StrategyID, o.UserID, string(o.Market), o.Symbol, o.Side, o.Price, o.Qty,
		o.ClientOID, o.ExchangeOrderID, o.Status, o.CreatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil {
		o.ID = id
	}
	return nil
}

func (s *mysqlStore) ListOrdersByStrategy(sid int64) ([]*BotOrder, error) {
	rows, err := s.db.Query(`SELECT id, strategy_id, user_id, market, symbol, side, price, qty, client_oid, exchange_order_id, status, created_at
		FROM ce_bot_orders WHERE strategy_id = ? ORDER BY id`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func (s *mysqlStore) CountOrdersByStrategy(sid int64) (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ce_bot_orders WHERE strategy_id = ?`, sid).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ---- 扫描辅助 ----

func scanStrategy(row *sql.Row) (*BotStrategy, error) {
	var st BotStrategy
	var market, side, typ, status, token, params string
	var gridStateJSON sql.NullString
	err := row.Scan(&st.ID, &st.UserID, &st.Name, &market, &st.Symbol, &side, &typ,
		&params, &status, &gridStateJSON, &token, &st.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrStrategyNotFound
	}
	if err != nil {
		return nil, err
	}
	st.Market = Market(market)
	st.Side = side
	st.Type = StrategyType(typ)
	st.Status = StrategyStatus(status)
	st.UserToken = token
	_ = json.Unmarshal([]byte(params), &st.Params)
	if gridStateJSON.Valid && gridStateJSON.String != "" {
		st.GridState = &GridState{}
		_ = json.Unmarshal([]byte(gridStateJSON.String), st.GridState)
	}
	return &st, nil
}

func scanStrategies(rows *sql.Rows) ([]*BotStrategy, error) {
	out := make([]*BotStrategy, 0)
	for rows.Next() {
		var st BotStrategy
		var market, side, typ, status, token, params string
		var gridStateJSON sql.NullString
		if err := rows.Scan(&st.ID, &st.UserID, &st.Name, &market, &st.Symbol, &side, &typ,
			&params, &status, &gridStateJSON, &token, &st.CreatedAt); err != nil {
			return nil, err
		}
		st.Market = Market(market)
		st.Side = side
		st.Type = StrategyType(typ)
		st.Status = StrategyStatus(status)
		st.UserToken = token
		_ = json.Unmarshal([]byte(params), &st.Params)
		if gridStateJSON.Valid && gridStateJSON.String != "" {
			st.GridState = &GridState{}
			_ = json.Unmarshal([]byte(gridStateJSON.String), st.GridState)
		}
		out = append(out, &st)
	}
	return out, rows.Err()
}

func scanOrders(rows *sql.Rows) ([]*BotOrder, error) {
	out := make([]*BotOrder, 0)
	for rows.Next() {
		var o BotOrder
		var market, side, status string
		if err := rows.Scan(&o.ID, &o.StrategyID, &o.UserID, &market, &o.Symbol, &side,
			&o.Price, &o.Qty, &o.ClientOID, &o.ExchangeOrderID, &status, &o.CreatedAt); err != nil {
			return nil, err
		}
		o.Market = Market(market)
		o.Side = side
		o.Status = status
		out = append(out, &o)
	}
	return out, rows.Err()
}
