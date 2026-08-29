-- ============================================================================
-- crypto-exchange · 数据库初始化脚本（init_db）
-- 生成时间：2026-08-27
--
-- 用途：在新环境首次部署时，一次性建好 crypto-exchange 各业务模块所需的全部数据表。
--
-- 重要：本文件各表的结构（列名、类型、默认值）严格对应代码 internal/**/store*_mysql.go
--        与 store_migrations.go 中的 migrate.Migration 定义（含所有 ALTER 定点化后的最终状态）。
--        需与代码保持同步；如代码迁移有变更，请同步更新此处。
--
-- 执行方式（任选其一）：
--   1) mysql -h<host> -u<user> -p<dbname> < scripts/init_db.sql
--   2) 在数据库控制台直接粘贴执行本文件内容。
--
-- 说明：
--   * 所有语句均为 CREATE TABLE IF NOT EXISTS，幂等，重复执行安全。
--   * 已应用的迁移版本（各模块 migrate.Migration 的 Version）记录在 ce_schema_migrations；
--     本脚本手动建表后，各服务再次启动时 migrate.Up() 会因版本已存在而跳过，不会冲突。
--   * staking/bot/copytrade 模块的原始建表语句未指定 ENGINE/CHARSET（与代码一致），其余模块为 utf8mb4。
-- ============================================================================

-- ---------------------------------------------------------------------------
-- 0) 迁移版本记录表（migrate 包内部使用，所有模块共用）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_schema_migrations (
    version    INT         NOT NULL,
    name       VARCHAR(128) NOT NULL,
    applied_at DATETIME(3) NOT NULL,
    PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 1) user 模块（迁移版本 9101-9107）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_users (
    id             BIGINT        NOT NULL AUTO_INCREMENT,
    email          VARCHAR(191)  NOT NULL DEFAULT '',
    phone          VARCHAR(32)   NOT NULL DEFAULT '',
    pass_hash      VARCHAR(255)  NOT NULL,
    status         TINYINT       NOT NULL DEFAULT 0,
    kyc_level      TINYINT       NOT NULL DEFAULT 0,
    tfa_secret     VARCHAR(255)  NULL,
    tfa_enabled    TINYINT       NOT NULL DEFAULT 0,
    email_verified TINYINT       NOT NULL DEFAULT 0,
    phone_verified TINYINT       NOT NULL DEFAULT 0,
    nickname       VARCHAR(64)   NOT NULL DEFAULT '',
    avatar         VARCHAR(512)  NOT NULL DEFAULT '',
    created_at     DATETIME(3)   NOT NULL,
    updated_at     DATETIME(3)   NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_email (email),
    UNIQUE KEY uk_phone (phone)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_user_codes (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    user_id    BIGINT       NOT NULL DEFAULT 0,
    target     VARCHAR(191) NOT NULL,
    purpose    VARCHAR(32)  NOT NULL,
    code       VARCHAR(32)  NOT NULL,
    expires_at DATETIME(3)  NOT NULL,
    consumed   TINYINT      NOT NULL DEFAULT 0,
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_target_purpose (target, purpose)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_user_refresh (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    user_id    BIGINT       NOT NULL,
    token_hash VARCHAR(255) NOT NULL,
    expires_at DATETIME(3)  NOT NULL,
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_token_hash (token_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_user_kyc (
    user_id       BIGINT       NOT NULL,
    real_name     VARCHAR(128) NOT NULL,
    id_type       VARCHAR(32)  NOT NULL,
    id_number     VARCHAR(128) NOT NULL,
    doc_front     VARCHAR(512) NULL,
    doc_back      VARCHAR(512) NULL,
    status        TINYINT      NOT NULL DEFAULT 1,
    reject_reason VARCHAR(255) NULL,
    submitted_at  DATETIME(3)  NOT NULL,
    reviewed_at   DATETIME(3)  NULL,
    reviewer      VARCHAR(128) NULL,
    PRIMARY KEY (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_user_preferences (
    user_id          BIGINT       NOT NULL,
    language         VARCHAR(32)  NOT NULL DEFAULT 'zh-CN',
    theme            VARCHAR(32)  NOT NULL DEFAULT 'light',
    notify_order     TINYINT      NOT NULL DEFAULT 1,
    notify_security  TINYINT      NOT NULL DEFAULT 1,
    notify_marketing TINYINT      NOT NULL DEFAULT 0,
    timezone         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'IANA 时区；空字符串表示跟随系统',
    updated_at       DATETIME(3)  NOT NULL,
    PRIMARY KEY (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 2) admin 模块（迁移版本 9201-9205，含 9204 登录锁定 ALTER 定点化列）
--    结构与 internal/adminapi/store_adminaccount_mysql.go 的 AdminMigrations 完全一致。
--    注意：status 为 VARCHAR(16)，仅 'active' 可登录（'pending'/'disabled' 不可）。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_admin_accounts (
    id              BIGINT      NOT NULL AUTO_INCREMENT,
    username        VARCHAR(64) NOT NULL,
    pass_hash       VARCHAR(255) NOT NULL,
    status          VARCHAR(16) NOT NULL DEFAULT 'pending',
    role_id         BIGINT      NOT NULL DEFAULT 0,
    totp_secret     VARCHAR(255) NULL,
    totp_enabled    TINYINT     NOT NULL DEFAULT 0,
    created_at      DATETIME(3) NOT NULL,
    updated_at      DATETIME(3) NOT NULL,
    failed_attempts INT         NOT NULL DEFAULT 0,
    locked_until    BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (id),
    UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_admin_roles (
    id          BIGINT      NOT NULL AUTO_INCREMENT,
    name        VARCHAR(64) NOT NULL,
    description VARCHAR(255) NOT NULL DEFAULT '',
    created_at  DATETIME(3) NOT NULL,
    updated_at  DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_role_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 权限以 "resource:action" 字符串存于 perm_key（对齐 SetRolePermissions 的写入方式）。
CREATE TABLE IF NOT EXISTS ce_admin_role_perms (
    role_id  BIGINT      NOT NULL,
    perm_key VARCHAR(64) NOT NULL,
    PRIMARY KEY (role_id, perm_key),
    KEY idx_perm_key (perm_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_admin_preferences (
    admin_id   BIGINT      NOT NULL,
    language   VARCHAR(32) NOT NULL DEFAULT 'zh-CN',
    theme      VARCHAR(32) NOT NULL DEFAULT 'light',
    timezone   VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'IANA 时区；空字符串表示跟随系统',
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (admin_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_admin_audit_logs (
    id       BIGINT      NOT NULL AUTO_INCREMENT,
    admin_id BIGINT      NOT NULL,
    method   VARCHAR(8)  NOT NULL,
    path     VARCHAR(255) NOT NULL,
    action   VARCHAR(16) NOT NULL DEFAULT '',
    target   VARCHAR(255) NOT NULL DEFAULT '',
    status   INT         NOT NULL DEFAULT 0,
    detail   VARCHAR(255) NOT NULL DEFAULT '',
    ip       VARCHAR(64) NOT NULL DEFAULT '',
    time     BIGINT      NOT NULL,
    PRIMARY KEY (id),
    KEY idx_time (time),
    KEY idx_admin (admin_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 2.1) 默认三角色 + 权限 + 引导管理员账户（幂等，可重复执行）
--   角色固定 id：1=super_admin, 2=admin, 3=operator（与 SeedBootstrap 写入顺序一致）
--   账户 username='admin'，密码见部署文档（bcrypt 哈希，bcrypt.DefaultCost），role_id=2
-- ---------------------------------------------------------------------------
INSERT IGNORE INTO ce_admin_roles (id, name, description, created_at, updated_at)
VALUES
    (1, 'super_admin', '超级管理员（全部权限）', NOW(3), NOW(3)),
    (2, 'admin',       '运营管理员（不含系统管理）', NOW(3), NOW(3)),
    (3, 'operator',    '只读操作员', NOW(3), NOW(3));

-- super_admin：全部权限
INSERT IGNORE INTO ce_admin_role_perms (role_id, perm_key) VALUES
    (1, 'dashboard:view'),
    (1, 'user:read'), (1, 'user:write'),
    (1, 'symbol:read'), (1, 'symbol:write'),
    (1, 'chain:read'), (1, 'chain:write'),
    (1, 'coin:read'), (1, 'coin:write'),
    (1, 'deposit:read'), (1, 'withdraw:approval'),
    (1, 'trade:read'), (1, 'trade:manage'),
    (1, 'notification:manage'),
    (1, 'ledger:read'), (1, 'service:read'),
    (1, 'apikey:read'), (1, 'apikey:manage'),
    (1, 'admin:manage'), (1, 'role:manage'),
    (1, 'audit:read');

-- admin（运营管理员）：不含系统管理（admin:manage/role:manage/audit:read 之外的高危项仍受限）
INSERT IGNORE INTO ce_admin_role_perms (role_id, perm_key) VALUES
    (2, 'dashboard:view'),
    (2, 'user:read'), (2, 'user:write'),
    (2, 'symbol:read'), (2, 'symbol:write'),
    (2, 'chain:read'), (2, 'chain:write'),
    (2, 'coin:read'), (2, 'coin:write'),
    (2, 'deposit:read'), (2, 'withdraw:approval'),
    (2, 'trade:read'), (2, 'trade:manage'),
    (2, 'notification:manage'),
    (2, 'ledger:read'), (2, 'service:read'),
    (2, 'apikey:read'), (2, 'apikey:manage');

-- operator（只读操作员）
INSERT IGNORE INTO ce_admin_role_perms (role_id, perm_key) VALUES
    (3, 'dashboard:view'),
    (3, 'user:read'),
    (3, 'symbol:read'), (3, 'chain:read'), (3, 'coin:read'),
    (3, 'deposit:read'), (3, 'ledger:read'), (3, 'service:read'),
    (3, 'trade:read'), (3, 'apikey:read');

-- 引导管理员账户（密码为其 bcrypt 哈希；status='active' 方可登录）
INSERT IGNORE INTO ce_admin_accounts
    (id, username, pass_hash, status, role_id, totp_enabled, created_at, updated_at, failed_attempts, locked_until)
VALUES
    (1, 'admin', '$2a$10$25Ob/Ghqo45Y920zWSQ/3.jx73CWb57II1foRKVSAlBfVWWUzYCmW',
     'active', 2, 0, NOW(3), NOW(3), 0, 0);

-- ---------------------------------------------------------------------------
-- 2.5) admin catalog 模块（迁移版本 9101-9104）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_admin_symbols (
    symbol       VARCHAR(32)  NOT NULL,
    base         VARCHAR(16)  NOT NULL DEFAULT '',
    quote        VARCHAR(16)  NOT NULL DEFAULT '',
    status       VARCHAR(16)  NOT NULL DEFAULT 'online',
    fee_rate     DOUBLE       NOT NULL DEFAULT 0,
    max_leverage INT          NOT NULL DEFAULT 0,
    min_qty      DOUBLE       NOT NULL DEFAULT 0,
    PRIMARY KEY (symbol)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_admin_chains (
    id              BIGINT      NOT NULL AUTO_INCREMENT,
    name            VARCHAR(64) NOT NULL DEFAULT '',
    symbol          VARCHAR(16) NOT NULL DEFAULT '',
    confirmations   INT         NOT NULL DEFAULT 0,
    deposit_enabled TINYINT     NOT NULL DEFAULT 1,
    withdraw_enabled TINYINT    NOT NULL DEFAULT 0,
    updated_at      DATETIME(3) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_admin_coins (
    id           BIGINT      NOT NULL AUTO_INCREMENT,
    symbol       VARCHAR(16) NOT NULL DEFAULT '',
    name         VARCHAR(64) NOT NULL DEFAULT '',
    chain        VARCHAR(32) NOT NULL DEFAULT '',
    decimals     INT         NOT NULL DEFAULT 0,
    withdraw_fee DOUBLE      NOT NULL DEFAULT 0,
    updated_at   DATETIME(3) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_admin_notifications (
    id         BIGINT      NOT NULL AUTO_INCREMENT,
    title      VARCHAR(255) NOT NULL DEFAULT '',
    body       TEXT,
    level      VARCHAR(16) NOT NULL DEFAULT 'info',
    created_at DATETIME(3) NOT NULL,
    source     VARCHAR(16) NOT NULL DEFAULT 'local',
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 3) apikeys 模块（迁移版本 9802）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_admin_api_keys (
    id           BIGINT       NOT NULL AUTO_INCREMENT,
    user_id      BIGINT       NOT NULL,
    label        VARCHAR(64)  NOT NULL,
    prefix       VARCHAR(16)  NOT NULL,
    key_hash     VARCHAR(64)  NOT NULL,
    permissions  TEXT         NOT NULL,
    status       VARCHAR(16)  NOT NULL DEFAULT 'active',
    created_by   BIGINT       NOT NULL DEFAULT 0,
    created_at   BIGINT       NOT NULL,
    last_used_at BIGINT       NULL,
    revoked_at   BIGINT       NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_key_hash (key_hash),
    KEY idx_user (user_id),
    KEY idx_prefix (prefix)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 4) risk 模块（迁移版本 9401-9404，含定点化 ALTER 后状态）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_risk_rules (
    id                 BIGINT       NOT NULL AUTO_INCREMENT,
    name               VARCHAR(128) NOT NULL DEFAULT '',
    kind               VARCHAR(32)  NOT NULL,
    scope              VARCHAR(16)  NOT NULL DEFAULT 'global',
    user_id            BIGINT       NOT NULL DEFAULT 0,
    asset              VARCHAR(32)  NOT NULL DEFAULT '',
    max_amount_per_day VARCHAR(64)  NOT NULL DEFAULT '0',
    max_count_per_day  INT          NOT NULL DEFAULT 0,
    min_kyc_level      INT          NOT NULL DEFAULT 0,
    enabled            TINYINT(1)   NOT NULL DEFAULT 1,
    created_at         DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_kind (kind)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_risk_blacklist (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    target     VARCHAR(128) NOT NULL,
    kind       VARCHAR(16)  NOT NULL,
    reason     VARCHAR(255) NOT NULL DEFAULT '',
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_target_kind (target, kind)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_risk_events (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    user_id    BIGINT       NOT NULL DEFAULT 0,
    kind       VARCHAR(32)  NOT NULL,
    detail     TEXT,
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_user (user_id),
    KEY idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 5) notification 模块（迁移版本 9301）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_notifications (
    id          BIGINT       NOT NULL AUTO_INCREMENT,
    user_id     BIGINT       NOT NULL,
    type        VARCHAR(32)  NOT NULL DEFAULT '',
    title       VARCHAR(255) NOT NULL DEFAULT '',
    body        TEXT,
    status      VARCHAR(16)  NOT NULL DEFAULT 'unread',
    created_at  DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_user_status (user_id, status),
    KEY idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 6) announcement 模块（迁移版本 9401）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_announcements (
    id           BIGINT       NOT NULL AUTO_INCREMENT,
    level        VARCHAR(16)  NOT NULL DEFAULT 'info',
    title        VARCHAR(128) NOT NULL,
    content      TEXT         NULL,
    active       TINYINT      NOT NULL DEFAULT 0,
    published_at DATETIME(3)  NULL,
    created_at   DATETIME(3)  NOT NULL,
    updated_at   DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_active_published (active, published_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 7) ledger 模块（迁移版本 1, 2）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_ledger_snapshots (
    id         VARCHAR(64)  NOT NULL,
    data       MEDIUMTEXT   NOT NULL,
    updated_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_ledger_idempotency (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    ledger_id  VARCHAR(64)  NOT NULL,
    kind       VARCHAR(16)  NOT NULL,
    fp         VARCHAR(512) NOT NULL,
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uniq_ledger_kind_fp (ledger_id, kind, fp)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 8) settlement 模块（迁移版本 9801）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_settlement_trades (
    id          BIGINT       NOT NULL,
    symbol      VARCHAR(32)  NOT NULL,
    price       DOUBLE       NOT NULL,
    qty         DOUBLE       NOT NULL,
    taker_id    BIGINT       NOT NULL,
    maker_id    BIGINT       NOT NULL,
    taker_side  VARCHAR(8)   NOT NULL,
    fee         DOUBLE       NOT NULL,
    ts          BIGINT       NOT NULL,
    cleared_at  DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_symbol (symbol),
    KEY idx_cleared_at (cleared_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 9) margin 模块（迁移版本 9201, 9202，含定点化 ALTER 后状态）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_margin_accounts (
    user_id            BIGINT        NOT NULL,
    asset              VARCHAR(32)   NOT NULL,
    collateral_asset   VARCHAR(32)   NOT NULL DEFAULT '',
    collateral_amount  VARCHAR(64)   NOT NULL DEFAULT '0',
    debt               VARCHAR(64)   NOT NULL DEFAULT '0',
    interest_accrued   VARCHAR(64)   NOT NULL DEFAULT '0',
    leverage           INT           NOT NULL DEFAULT 1,
    status             VARCHAR(16)   NOT NULL DEFAULT 'active',
    last_accrual       DATETIME(3)   NOT NULL,
    created_at         DATETIME(3)   NOT NULL,
    updated_at         DATETIME(3)   NOT NULL,
    PRIMARY KEY (user_id, asset)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 10) options 模块（迁移版本 9501-9503，含定点化 ALTER 后状态）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_option_contracts (
    id            BIGINT         NOT NULL AUTO_INCREMENT,
    underlying    VARCHAR(32)    NOT NULL,
    quote_asset   VARCHAR(32)    NOT NULL DEFAULT '',
    strike        DOUBLE         NOT NULL DEFAULT 0,
    expiry        DATETIME(3)    NOT NULL,
    type          VARCHAR(8)     NOT NULL,
    style         VARCHAR(16)    NOT NULL,
    contract_size DOUBLE         NOT NULL DEFAULT 1,
    premium       DOUBLE         NOT NULL DEFAULT 0,
    created_at    DATETIME(3)    NOT NULL,
    updated_at    DATETIME(3)    NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_option_positions (
    id           BIGINT        NOT NULL AUTO_INCREMENT,
    user_id      BIGINT        NOT NULL,
    contract_id  BIGINT        NOT NULL,
    side         VARCHAR(8)    NOT NULL,
    quantity     DOUBLE        NOT NULL DEFAULT 0,
    quote_asset  VARCHAR(16)   NOT NULL DEFAULT '',
    premium      VARCHAR(64)   NOT NULL DEFAULT '0',
    margin       VARCHAR(64)   NOT NULL DEFAULT '0',
    status       VARCHAR(16)   NOT NULL DEFAULT 'open',
    opened_at    DATETIME(3)   NOT NULL,
    updated_at   DATETIME(3)   NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 11) otc 模块（迁移版本 9601-9606，含定点化 ALTER 后状态）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_otc_advertisements (
    id             BIGINT        NOT NULL AUTO_INCREMENT,
    user_id        BIGINT        NOT NULL,
    side           VARCHAR(8)    NOT NULL,
    asset          VARCHAR(32)   NOT NULL,
    fiat_currency  VARCHAR(8)    NOT NULL DEFAULT '',
    price          DOUBLE        NOT NULL DEFAULT 0,
    min_amount     DOUBLE        NOT NULL DEFAULT 0,
    max_amount     DOUBLE        NOT NULL DEFAULT 0,
    payment_methods VARCHAR(255) NOT NULL DEFAULT '',
    status         VARCHAR(16)   NOT NULL DEFAULT 'open',
    created_at     DATETIME(3)   NOT NULL,
    updated_at     DATETIME(3)   NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_user (user_id),
    INDEX idx_side_asset (side, asset)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_otc_orders (
    id             BIGINT        NOT NULL AUTO_INCREMENT,
    ad_id          BIGINT        NOT NULL,
    maker_id       BIGINT        NOT NULL,
    taker_id       BIGINT        NOT NULL,
    side           VARCHAR(8)    NOT NULL,
    asset          VARCHAR(32)   NOT NULL,
    fiat_currency  VARCHAR(8)    NOT NULL DEFAULT '',
    crypto_amount  VARCHAR(64)   NOT NULL DEFAULT '0',
    price          DOUBLE        NOT NULL DEFAULT 0,
    fiat_amount    DOUBLE        NOT NULL DEFAULT 0,
    payment_method VARCHAR(64)   NOT NULL DEFAULT '',
    status         VARCHAR(16)   NOT NULL DEFAULT 'pending',
    rating         INT           NOT NULL DEFAULT 0,
    created_at     DATETIME(3)   NOT NULL,
    paid_at        DATETIME(3)   NULL DEFAULT NULL,
    completed_at   DATETIME(3)   NULL DEFAULT NULL,
    updated_at     DATETIME(3)   NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_maker (maker_id),
    INDEX idx_taker (taker_id),
    INDEX idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_otc_counterparties (
    id              BIGINT        NOT NULL AUTO_INCREMENT,
    user_id         BIGINT        NOT NULL,
    counterparty_id BIGINT        NOT NULL,
    trades_total    INT           NOT NULL DEFAULT 0,
    trades_completed INT          NOT NULL DEFAULT 0,
    rating_sum      INT           NOT NULL DEFAULT 0,
    rating_count    INT           NOT NULL DEFAULT 0,
    created_at      DATETIME(3)   NOT NULL,
    updated_at      DATETIME(3)   NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uniq_pair (user_id, counterparty_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_otc_messages (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    order_id   BIGINT       NOT NULL,
    sender_id  BIGINT       NOT NULL,
    content    TEXT         NOT NULL,
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_order (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_otc_proofs (
    id           BIGINT       NOT NULL AUTO_INCREMENT,
    order_id     BIGINT       NOT NULL,
    uploader_id  BIGINT       NOT NULL,
    file_name    VARCHAR(255) NOT NULL DEFAULT '',
    content_type VARCHAR(128) NOT NULL DEFAULT '',
    size         BIGINT       NOT NULL DEFAULT 0,
    url          VARCHAR(512) NOT NULL DEFAULT '',
    created_at   DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_order (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 12) wealth 模块（迁移版本 9701-9703，含定点化 ALTER 后状态）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_wealth_products (
    id             BIGINT        NOT NULL AUTO_INCREMENT,
    name           VARCHAR(128)  NOT NULL DEFAULT '',
    asset          VARCHAR(32)   NOT NULL,
    type           VARCHAR(16)   NOT NULL,
    annual_rate    DOUBLE        NOT NULL DEFAULT 0,
    duration_days  INT           NOT NULL DEFAULT 0,
    min_amount     DOUBLE        NOT NULL DEFAULT 0,
    status         VARCHAR(16)   NOT NULL DEFAULT 'open',
    created_at     DATETIME(3)   NOT NULL,
    updated_at     DATETIME(3)   NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_wealth_holdings (
    id              BIGINT        NOT NULL AUTO_INCREMENT,
    user_id         BIGINT        NOT NULL,
    product_id      BIGINT        NOT NULL,
    asset           VARCHAR(32)   NOT NULL DEFAULT '',
    principal       VARCHAR(64)   NOT NULL DEFAULT '0',
    accrued_yield   VARCHAR(64)   NOT NULL DEFAULT '0',
    status          VARCHAR(16)   NOT NULL DEFAULT 'active',
    created_at      DATETIME(3)   NOT NULL,
    last_accrual_at DATETIME(3)   NOT NULL,
    redeemed_at     DATETIME(3)   NULL DEFAULT NULL,
    updated_at      DATETIME(3)   NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_user (user_id),
    INDEX idx_product (product_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 13) staking 模块（迁移版本 9801, 9802；原始建表未指定 ENGINE/CHARSET，与代码一致）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_staking_products (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    name VARCHAR(128) NOT NULL,
    chain VARCHAR(32) NOT NULL,
    validator VARCHAR(128) NOT NULL,
    contract_addr VARCHAR(128) NOT NULL,
    asset VARCHAR(16) NOT NULL,
    annual_rate DOUBLE NOT NULL DEFAULT 0,
    duration_days INT NOT NULL DEFAULT 0,
    min_amount BIGINT NOT NULL DEFAULT 0,
    min_amount_decimals INT NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL DEFAULT 'active',
    created_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS ce_staking_delegations (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    user_id BIGINT NOT NULL,
    product_id BIGINT NOT NULL,
    principal BIGINT NOT NULL,
    principal_decimals INT NOT NULL,
    status VARCHAR(16) NOT NULL,
    tx_hash VARCHAR(128) NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    unbond_at BIGINT NOT NULL DEFAULT 0,
    unbonded_at BIGINT NOT NULL DEFAULT 0,
    INDEX idx_user (user_id),
    INDEX idx_product (product_id)
);

CREATE TABLE IF NOT EXISTS ce_staking_rewards (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    delegation_id BIGINT NOT NULL,
    amount BIGINT NOT NULL,
    amount_decimals INT NOT NULL,
    accrued_at BIGINT NOT NULL,
    INDEX idx_delegation (delegation_id)
);

-- ---------------------------------------------------------------------------
-- 14) bot 模块（迁移版本 9803, 9804；原始建表未指定 ENGINE/CHARSET，与代码一致）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_bot_strategies (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    user_id BIGINT NOT NULL,
    name VARCHAR(128) NOT NULL,
    market VARCHAR(16) NOT NULL,
    symbol VARCHAR(32) NOT NULL,
    side VARCHAR(8) NOT NULL,
    type VARCHAR(16) NOT NULL,
    params JSON NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'stopped',
    user_token VARCHAR(512) NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    INDEX idx_user (user_id)
);

CREATE TABLE IF NOT EXISTS ce_bot_orders (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    strategy_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    market VARCHAR(16) NOT NULL,
    symbol VARCHAR(32) NOT NULL,
    side VARCHAR(8) NOT NULL,
    price DOUBLE NOT NULL DEFAULT 0,
    qty DOUBLE NOT NULL DEFAULT 0,
    client_oid VARCHAR(128) NOT NULL DEFAULT '',
    exchange_order_id VARCHAR(128) NOT NULL DEFAULT '',
    status VARCHAR(16) NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    INDEX idx_strategy (strategy_id)
);

-- ---------------------------------------------------------------------------
-- 15) copytrade 模块（迁移版本 9805-9808，含定点化 ALTER 后状态；原始建表未指定 ENGINE/CHARSET）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_copytrade_leads (
    id BIGINT PRIMARY KEY,
    name VARCHAR(128) NOT NULL DEFAULT '',
    bio VARCHAR(512) NOT NULL DEFAULT '',
    status VARCHAR(16) NOT NULL DEFAULT 'active',
    created_at BIGINT NOT NULL,
    INDEX idx_status (status)
);

CREATE TABLE IF NOT EXISTS ce_copytrade_follows (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    lead_id BIGINT NOT NULL,
    follower_id BIGINT NOT NULL,
    copy_ratio DOUBLE NOT NULL DEFAULT 1,
    allocated_amount DOUBLE NOT NULL DEFAULT 0,
    follower_token VARCHAR(512) NOT NULL DEFAULT '',
    status VARCHAR(16) NOT NULL DEFAULT 'active',
    created_at BIGINT NOT NULL,
    stopped_at BIGINT NOT NULL DEFAULT 0,
    INDEX idx_lead (lead_id),
    INDEX idx_follower (follower_id)
);

CREATE TABLE IF NOT EXISTS ce_copytrade_copies (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    event_id VARCHAR(128) NOT NULL,
    lead_id BIGINT NOT NULL,
    follow_id BIGINT NOT NULL,
    follower_id BIGINT NOT NULL,
    symbol VARCHAR(32) NOT NULL,
    side VARCHAR(8) NOT NULL,
    price DOUBLE NOT NULL DEFAULT 0,
    qty DOUBLE NOT NULL DEFAULT 0,
    notional DOUBLE NOT NULL DEFAULT 0,
    fee_amount DOUBLE NOT NULL DEFAULT 0,
    fee_value BIGINT NOT NULL DEFAULT 0,
    fee_decimals INT NOT NULL DEFAULT 0,
    exchange_order_id VARCHAR(128) NOT NULL DEFAULT '',
    status VARCHAR(16) NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    UNIQUE KEY uniq_event_follow (event_id, follow_id),
    INDEX idx_follower (follower_id)
);

-- ---------------------------------------------------------------------------
-- 16) spot 模块（迁移版本 9801）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_spot_orders (
    order_id         BIGINT       NOT NULL,
    user_id          BIGINT       NOT NULL,
    side             INT         NOT NULL,
    symbol           VARCHAR(32) NOT NULL,
    base             VARCHAR(32) NOT NULL,
    quote            VARCHAR(32) NOT NULL,
    frozen_quote_val VARCHAR(64) NOT NULL DEFAULT '0',
    frozen_quote_dec INT         NOT NULL DEFAULT 0,
    frozen_base_val  VARCHAR(64) NOT NULL DEFAULT '0',
    frozen_base_dec  INT         NOT NULL DEFAULT 0,
    client_oid       VARCHAR(128) NOT NULL DEFAULT '',
    created_at       DATETIME(3) NOT NULL,
    PRIMARY KEY (order_id),
    INDEX idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- 17) matching 模块（迁移版本 200-203；202/203 迁移内含初始数据 INSERT）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ce_matching_wal (
    seq        BIGINT       NOT NULL AUTO_INCREMENT,
    symbol     VARCHAR(32)  NOT NULL,
    event_type VARCHAR(16)  NOT NULL,
    payload    JSON,
    ts         BIGINT       NOT NULL,
    PRIMARY KEY (seq),
    INDEX idx_matching_wal_symbol (symbol)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_matching_snapshot (
    id         INT          NOT NULL,
    version    BIGINT       NOT NULL,
    state      LONGBLOB,
    updated_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ce_matching_seq (
    id  INT    NOT NULL,
    val BIGINT NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO ce_matching_seq (id, val) VALUES (1, 0) ON DUPLICATE KEY UPDATE id = id;

CREATE TABLE IF NOT EXISTS ce_matching_leader (
    id         INT          NOT NULL,
    holder     VARCHAR(128) NOT NULL,
    expires_at DATETIME(3)  NOT NULL,
    heartbeat  DATETIME(3)  NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO ce_matching_leader (id, holder, expires_at, heartbeat)
VALUES (1, '', '1970-01-01 00:00:00', '1970-01-01 00:00:00')
ON DUPLICATE KEY UPDATE id = id;

-- ============================================================================
-- 初始化完成。执行后可用以下语句核对：
--   SHOW TABLES LIKE 'ce_%';
-- 预期应包含 ce_admin_accounts 等全部 ce_ 前缀表。
--
-- 注意：本脚本已包含各表迁移（含定点化 ALTER）后的最终结构，与代码运行时 schema 一致。
-- 若代码迁移又有新的 ALTER/建表，请同步更新本文件，否则各服务启动时 migrate.Up()
-- 仍会按缺失的版本号自动应用剩余迁移，不影响功能，但本脚本将不再代表完整最终状态。
-- ============================================================================
