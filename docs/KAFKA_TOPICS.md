# Kafka Topic 设计

> 对应开发任务 T-12。撮合成交通过 `internal/pkg/mq` 发布到消息总线（见 DEVELOPMENT_TASKS.md T-02），
> 本文件定义各 topic 的用途、消息 schema 与分区/保留策略建议。生产建议启用 `-tags kafka` 构建以使用
> `KafkaPublisher`。

## 命名规范

- topic 全小写，点号分段：`<域>.<实体>.<动作>`，例如 `exchange.trades`、`wallet.deposits`。
- 所有事件消息建议带公共信封字段：`ts`（毫秒）、`trace_id`、`source`（服务名）。

## Topic 清单

| Topic | 生产者 | 消费者 | 用途 | 关键字段 | 建议分区 | 保留 |
|-------|--------|--------|------|----------|----------|------|
| `exchange.orders` | 网关 / 各交易服务 | 撮合引擎 | 新订单流入 | order_id, user_id, symbol, side, price, qty, type | 按 symbol 哈希 12 | 7d |
| `exchange.trades` | 撮合引擎（`mq.TradeEvent`） | 清算服务、行情服务、风控 | 成交流（触发记账/行情/风控） | symbol, price, qty, taker_id, maker_id, taker_side, ts | 按 symbol 哈希 12 | 30d |
| `wallet.deposits` | 链上网关 | 钱包总账 | 链上充值上账 | user_id, asset, amount, tx_hash, chain | 6 | 永久 |
| `wallet.withdrawals` | 出金服务 | 链上网关 | 提现指令 | user_id, asset, amount, fee, address | 6 | 永久 |
| `wallet.reorgs` | 链上网关 | 钱包总账 | 区块重组回滚 | tx_hash, height, asset | 3 | 30d |
| `liquidation.events` | 强平引擎 | 行情/前端/审计 | 强平/ADL/社会化分摊事件 | user_id, symbol, side, reduced, profit_covered | 3 | 90d |
| `risk.events` | 风控引擎 | 风控处置/审计 | 风控触发与处置 | user_id, rule, action, severity | 3 | 90d |
| `market.depth` | 撮合引擎 | 行情服务 | 深度快照（节流） | symbol, bids[], asks[], ts | 按 symbol 哈希 6 | 1d |
| `metrics.snapshots` | 各服务 | 监控 | 业务/技术指标 | service, name, value, ts | 3 | 30d |

## 消息 Schema（以 `exchange.trades` 为例）

```json
{
  "symbol": "BTCUSDT",
  "price": 50000.12,
  "qty": 0.5,
  "taker_id": 1001,
  "maker_id": 2002,
  "taker_side": "buy",
  "ts": 1718000000000
}
```

与 `internal/pkg/mq.TradeEvent` 结构一致（JSON 编码）。建议用 Protobuf 编码以降低体积（见 proto 生成脚本）。

## 交付语义

- 至少一次（at-least-once）：消费者需幂等（按 order_id / trade ts+user 去重）。
- 清算服务消费 `exchange.trades` 后写账本，失败重试并告警，不阻塞撮合（发布端 best-effort）。
- 关键金融事件（`wallet.*`、`liquidation.*`）建议开启 acks=all + 副本因子≥3。
