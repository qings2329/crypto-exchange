// Package catalog 提供跨域共享的链配置只读查询，避免 settlement/futures 等核心服务
// 直接依赖 adminapi 的 HTTP/路由层（参照 internal/announcement 的跨域共享模式）。
//
// 当前仅暴露从 ce_admin_chains 读取 RPC 端点的能力，使链清结算层（settlement）的
// RPC 客户端以数据库为单一数据源，而非散落在 config.yaml。
package catalog

import "database/sql"

// LoadChainRPCEndpoints 从 ce_admin_chains 读取已配置的公链 RPC 端点，
// 返回 map[链 symbol]rpcURL（如 {"BTC": "http://user:pass@host:8332", ...}）。
//
// 仅返回 rpc_endpoint 非空的行；查询出错（如表/列尚不存在，迁移未应用）时返回
// 空 map 与 error，由调用方回退到 config.yaml 的 endpoints（防御脏状态，避免启动失败）。
func LoadChainRPCEndpoints(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT symbol, rpc_endpoint FROM ce_admin_chains WHERE rpc_endpoint <> ''`)
	if err != nil {
		return map[string]string{}, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var sym, url string
		if err := rows.Scan(&sym, &url); err != nil {
			return out, err
		}
		out[sym] = url
	}
	return out, rows.Err()
}
