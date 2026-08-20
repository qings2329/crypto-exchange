# AGENTS.md

## Build & Test

```bash
make build                     # build all services to bin/
make build futures             # build a single service
make test                      # go test ./...
make test-race                 # go test -race ./...
make test-cover                # go test -race -coverprofile + total coverage
make lint                      # go vet ./...
make proto                     # regenerate protobuf/gRPC stubs (requires protoc + plugins)
./scripts/e2e.sh               # cross-service e2e (bot/copytrade/staking/lending, no MySQL/Kafka needed)
./scripts/integration.sh       # binary-level integration test (builds, launches, drives HTTP assertions)
```

### Build tag: `kafka`

Kafka (sarama) is gated by the `kafka` build tag. Default builds exclude it. Only add `TAGS=kafka` when actually needing Kafka:
```bash
make build TAGS=kafka
go test -tags=kafka ./...
```

### `--mysql-dsn ""` = in-memory mode

Every service accepts `--mysql-dsn ""` which triggers in-memory stores (no MySQL required). The integration test script uses this to run the full stack without external deps. When DSN is empty and no fallback exists, services log a warning and continue with memory-only persistence.

### Running a single service locally

```bash
make run-futures               # go run -tags= ./cmd/futures
make run-spot                  # go run -tags= ./cmd/spot
# etc.
```

Services read `configs/config.yaml` by default. Override with `--config path`.

## Architecture

Go monorepo (module `github.com/coldlar/crypto-exchange`). Each business line is a separate service binary under `cmd/` with domain logic in `internal/`.

**Key boundaries:**
- `cmd/` — process assembly only (config, DI, signal handling, route registration). No business logic.
- `internal/pkg/` — shared infra: config, logger, middleware (auth/ratelimit/security), response helpers, mq (Kafka abstraction), redis, migrate, es, influxdb.
- `internal/<domain>/` — business logic per service. Each follows Handler/Service/Store/Model 4-layer separation.
- `internal/matching/` — order-matching engine (in-memory orderbook, single-goroutine per symbol).
- `internal/ledger/` — double-entry wallet ledger. All money movements go through here.
- `internal/futures/` — perpetual futures: positions, margin, liquidation, mark price, funding rate.
- `internal/lending/` — P2P lending: pools, lend/borrow orders, interest accrual, collateral management.
- `api/` — protobuf definitions; generated code goes to `internal/gen/`.

**Frontend is a separate repo** at `../ce-frontend/` (user) and `../web-admin/` (admin). Not in this tree.

**Service ports** are defined in `configs/config.yaml` under `services:`. Gateway listens on `:8080`.

## Conventions (enforced)

See `docs/CONVENTIONS.md` for full details. Critical rules:

1. **All DB table names must start with `ce_`** (e.g. `ce_ledger_snapshots`, `ce_users`).
2. **4-layer separation** in every service: Handler (HTTP binding only) → Service (business logic, depends on Store interface) → Store (persistence, interface + mem + mysql impls) → Model (data structs + domain errors). No cross-layer violations.
3. **Migration version numbers are globally unique** across all modules (single `ce_schema_migrations` table). Each module uses a distinct range (e.g. ledger=1, matching=2, ..., margin=9201, notification=9301, risk=9401, options=9501, otc=9601, wealth=9701, bot=9800+, lending=9901+).
4. All funds go through `internal/ledger` — idempotent, auditable entries.

## Testing

- **Unit tests**: most packages have `_test.go` files with in-memory stores. Run with `make test`.
- **E2E** (`e2e/e2e_test.go`): self-contained, no external deps. Tests bot/copytrade/staking/lending cross-service auth and idempotency via httptest.
- **Integration** (`scripts/integration.sh`): builds real binaries, launches in-memory mode, drives HTTP assertions. Requires `go`, `curl`, `python3`, `openssl`.
- **Kafka integration**: `internal/pkg/mq/kafka_integration_test.go` — gated by `kafka` tag + `KAFKA_BROKER` env var.
- **MySQL integration**: `internal/pkg/migrate/migrate_test.go` — gated by `MYSQL_TEST_DSN` env var.
- Known flaky: `internal/spot.TestWSPushSubscribedSymbol` (websocket race in test env).

## Gotchas

- `go.mod` requires **Go 1.25**. Check your local version.
- `trusted_proxies` in config is critical when behind a gateway/LB — without it, `c.ClientIP()` returns the proxy IP, breaking rate limiting and audit logging. Leave empty only if directly exposed.
- The `auth.secret` in config (`AUTH_SECRET` env override) is the HMAC key for bearer tokens. Dev default: `dev-only-change-me`.
- MySQL driver is pinned to **v1.7.1** — newer versions are incompatible with sqlpub proxy (caching_sha2 auth issue). See comments in `configs/config.yaml`.
- Kafka `version` field in config must match your broker; default is `"3.6.0"`.
- Secrets (RPC endpoints, signing keys, HSM credentials) should be injected via environment variables, never committed to config files. See `.env.example`.
