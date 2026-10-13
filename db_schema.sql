-- Merged baseline from migrations/db_schema_0001..0003.sql, in order. Do NOT edit by hand.

-- >>> BEGIN db_schema_0001.sql >>>
-- =====================================================================
-- FinanceApp — МОНОЛИТНАЯ версия (один PostgreSQL, без Kafka)
-- =====================================================================
-- Что изменено по сравнению с микросервисной схемой:
--   1. Все 8 схем живут в ОДНОЙ базе данных.
--   2. "Мягкие" UUID-ссылки между сервисами заменены на реальные
--      FOREIGN KEY — целостность теперь гарантирует сама БД.
--   3. Вместо 8 таблиц outbox + Kafka — единый журнал `app.events`
--      и PostgreSQL LISTEN/NOTIFY для мгновенной доставки событий
--      внутри процесса (триггеры). Таблица `app.events` остаётся
--      источником истины для отладки и пересборки аналитики.
--   4. RLS отключён (личное приложение, один пользователь).
--      В конце файла — закомментированный блок для включения обратно.
--   5. Добавлены:
--      - триггер автообновления updated_at;
--      - CHECK-ограничения на все псевдо-enum поля;
--      - поле to_account_id для переводов (второе плечо);
--      - представление для удобной выборки транзакций.
-- =====================================================================

-- ---------------------------------------------------------------------
-- 0. Расширения
-- ---------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS citext;     -- регистронезависимый email

-- При первом развёртывании в новой БД можно раскомментировать:
-- DROP SCHEMA IF EXISTS app       CASCADE;
-- DROP SCHEMA IF EXISTS identity  CASCADE;
-- DROP SCHEMA IF EXISTS fx        CASCADE;
-- DROP SCHEMA IF EXISTS account   CASCADE;
-- DROP SCHEMA IF EXISTS tx        CASCADE;
-- DROP SCHEMA IF EXISTS budget    CASCADE;
-- DROP SCHEMA IF EXISTS invest    CASCADE;
-- DROP SCHEMA IF EXISTS import    CASCADE;
-- DROP SCHEMA IF EXISTS analytics CASCADE;

-- ---------------------------------------------------------------------
-- 1. Создание схем
-- ---------------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS app;        -- служебная: события, утилиты
CREATE SCHEMA IF NOT EXISTS identity;   -- пользователи, организации, роли
CREATE SCHEMA IF NOT EXISTS fx;         -- валюты и курсы
CREATE SCHEMA IF NOT EXISTS account;    -- счета и балансы
CREATE SCHEMA IF NOT EXISTS tx;         -- транзакции, категории, теги
CREATE SCHEMA IF NOT EXISTS budget;     -- бюджеты и лимиты
CREATE SCHEMA IF NOT EXISTS invest;     -- инвестиционный портфель
CREATE SCHEMA IF NOT EXISTS import;     -- импорт CSV/OFX
CREATE SCHEMA IF NOT EXISTS analytics;  -- read-модели (дашборды)

-- =====================================================================
-- 2. СЛУЖЕБНАЯ СХЕМА app — единый журнал событий вместо Kafka
-- =====================================================================
-- Каждая значимая запись в бизнес-таблицах сопровождается строкой здесь
-- и вызовом pg_notify('app_events', ...). Любое приложение-воркер
-- может подписаться:  LISTEN app_events;
-- Таблица app.events — источник истины для отладки и пересборки
-- аналитических моделей.
-- =====================================================================

CREATE TABLE app.events (
    id             BIGSERIAL PRIMARY KEY,
    aggregate_type TEXT NOT NULL,        -- 'transaction', 'account', 'budget', ...
    aggregate_id   TEXT NOT NULL,        -- UUID или BIGINT приведённые к тексту
    event_type     TEXT NOT NULL,        -- 'transaction.created', 'account.balance-changed', ...
    payload        JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_app_events_created ON app.events(created_at DESC);
CREATE INDEX idx_app_events_type    ON app.events(event_type);

-- ---------------------------------------------------------------------
-- Функция публикации: пишет в журнал + отправляет NOTIFY.
-- Если payload слишком большой для ограничения NOTIFY (~8000 байт),
-- в уведомлении остаётся только тип и идентификатор агрегата,
-- а consumer сам дочитает данные по ссылке.
-- ---------------------------------------------------------------------
CREATE OR REPLACE FUNCTION app.publish_event(
    p_aggregate_type TEXT,
    p_aggregate_id   TEXT,
    p_event_type     TEXT,
    p_payload        JSONB
) RETURNS void AS $$
DECLARE
    v_notification JSONB;
BEGIN
    INSERT INTO app.events(aggregate_type, aggregate_id, event_type, payload)
    VALUES (p_aggregate_type, p_aggregate_id, p_event_type, p_payload);

    v_notification := jsonb_build_object(
        'event_type',     p_event_type,
        'aggregate_type', p_aggregate_type,
        'aggregate_id',   p_aggregate_id,
        'payload',        p_payload
    );

    -- Обрезаем до безопасного размера, если строка слишком длинная
    IF octet_length(v_notification::text) > 7000 THEN
        v_notification := jsonb_build_object(
            'event_type',     p_event_type,
            'aggregate_type', p_aggregate_type,
            'aggregate_id',   p_aggregate_id,
            'payload',        '{}'::jsonb,
            'truncated',      true
        );
    END IF;

    PERFORM pg_notify('app_events', v_notification::text);
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------
-- Универсальный триггер событий.
-- Аргумент 1: имя агрегата ('transaction', 'account', ...).
-- Аргумент 2: 'soft' если в таблице есть deleted_at, иначе 'hard'.
-- Использование:
--   CREATE TRIGGER trg_x
--     AFTER INSERT OR UPDATE OR DELETE ON some.table
--     FOR EACH ROW EXECUTE FUNCTION app.event_notify('aggregate','soft');
-- ---------------------------------------------------------------------
CREATE OR REPLACE FUNCTION app.event_notify() RETURNS trigger AS $$
DECLARE
    v_aggregate_type TEXT := TG_ARGV[0];
    v_soft           BOOLEAN := (TG_ARGV[1] = 'soft');
    v_event          TEXT;
    v_payload        JSONB;
    v_id             TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        v_id      := OLD.id::TEXT;
        v_event   := v_aggregate_type || '.deleted';
        v_payload := to_jsonb(OLD);
        PERFORM app.publish_event(v_aggregate_type, v_id, v_event, v_payload);
        RETURN OLD;
    END IF;

    v_id := NEW.id::TEXT;

    IF TG_OP = 'INSERT' THEN
        v_event := v_aggregate_type || '.created';
    ELSE
        IF v_soft
           AND NEW.deleted_at IS NOT NULL
           AND OLD.deleted_at IS NULL THEN
            v_event := v_aggregate_type || '.deleted';
        ELSE
            v_event := v_aggregate_type || '.updated';
        END IF;
    END IF;

    v_payload := to_jsonb(NEW);
    PERFORM app.publish_event(v_aggregate_type, v_id, v_event, v_payload);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------
-- Триггер автообновления updated_at
-- ---------------------------------------------------------------------
CREATE OR REPLACE FUNCTION app.set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- =====================================================================
-- 3. IDENTITY — пользователи, организации, роли, токены
-- =====================================================================

CREATE TABLE identity.organizations (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    plan          TEXT NOT NULL DEFAULT 'free'
                  CHECK (plan IN ('free','pro','team')),
    base_currency CHAR(3) NOT NULL DEFAULT 'USD',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE identity.users (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    email             CITEXT NOT NULL UNIQUE,
    password_hash     TEXT NOT NULL,
    display_name      TEXT,
    status            TEXT NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active','suspended','deleted')),
    email_verified_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ
);
CREATE INDEX idx_users_org ON identity.users(org_id);

CREATE TABLE identity.roles (
    id   SMALLSERIAL PRIMARY KEY,
    code TEXT NOT NULL UNIQUE
);

CREATE TABLE identity.user_roles (
    user_id UUID     NOT NULL REFERENCES identity.users(id) ON DELETE CASCADE,
    role_id SMALLINT NOT NULL REFERENCES identity.roles(id),
    PRIMARY KEY (user_id, role_id)
);

CREATE TABLE identity.refresh_tokens (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES identity.users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_refresh_tokens_user
    ON identity.refresh_tokens(user_id) WHERE revoked_at IS NULL;

-- =====================================================================
-- 4. FX — справочник валют и курсы
-- =====================================================================

CREATE TABLE fx.currencies (
    code       CHAR(3) PRIMARY KEY,
    name       TEXT NOT NULL,
    minor_unit SMALLINT NOT NULL DEFAULT 2
);

CREATE TABLE fx.rates (
    id         BIGSERIAL PRIMARY KEY,
    base_code  CHAR(3) NOT NULL REFERENCES fx.currencies(code),
    quote_code CHAR(3) NOT NULL REFERENCES fx.currencies(code),
    rate       NUMERIC(20,10) NOT NULL,
    as_of      DATE NOT NULL,
    source     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (base_code, quote_code, as_of)
);
CREATE INDEX idx_fx_rates_lookup ON fx.rates(base_code, quote_code, as_of DESC);

-- =====================================================================
-- 5. ACCOUNT — счета и баланс-леджер
-- =====================================================================

CREATE TABLE account.accounts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    owner_user_id UUID NOT NULL REFERENCES identity.users(id),
    name          TEXT NOT NULL,
    type          TEXT NOT NULL
                  CHECK (type IN ('cash','card','deposit','debt')),
    currency      CHAR(3) NOT NULL,
    balance_minor BIGINT NOT NULL DEFAULT 0,
    is_archived   BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);
CREATE INDEX idx_accounts_org   ON account.accounts(org_id);
CREATE INDEX idx_accounts_owner ON account.accounts(owner_user_id);

-- =====================================================================
-- 6. TRANSACTION — категории, транзакции, теги
-- =====================================================================

CREATE TABLE tx.categories (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    parent_id  UUID REFERENCES tx.categories(id) ON DELETE SET NULL,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('income','expense')),
    icon       TEXT,
    is_system  BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, parent_id, name)
);
CREATE INDEX idx_categories_org ON tx.categories(org_id);

CREATE TABLE tx.transactions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    account_id    UUID NOT NULL REFERENCES account.accounts(id),
    to_account_id UUID REFERENCES account.accounts(id),
    category_id   UUID REFERENCES tx.categories(id) ON DELETE SET NULL,
    kind          TEXT NOT NULL
                  CHECK (kind IN ('income','expense','transfer')),
    amount_minor  BIGINT NOT NULL CHECK (amount_minor > 0),
    currency      CHAR(3) NOT NULL,
    amount_in_account_currency_minor BIGINT,
    description   TEXT,
    occurred_at   TIMESTAMPTZ NOT NULL,
    source        TEXT NOT NULL DEFAULT 'manual'
                  CHECK (source IN ('manual','import','recurring')),
    import_job_id UUID,                  -- FK добавляется ниже после создания import.import_jobs
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ,
    -- Для перевода обязательно второе плечо и запрещена категория
    CHECK (kind <> 'transfer' OR to_account_id IS NOT NULL),
    CHECK (kind <> 'transfer' OR category_id IS NULL),
    -- Перевод не может быть на тот же счёт
    CHECK (kind <> 'transfer' OR account_id <> to_account_id)
);
CREATE INDEX idx_tx_org_occurred      ON tx.transactions(org_id, occurred_at DESC);
CREATE INDEX idx_tx_account_occurred  ON tx.transactions(account_id, occurred_at DESC);
CREATE INDEX idx_tx_category_occurred ON tx.transactions(category_id, occurred_at DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE tx.tags (
    id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    name   TEXT NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE tx.transaction_tags (
    transaction_id UUID NOT NULL REFERENCES tx.transactions(id) ON DELETE CASCADE,
    tag_id         UUID NOT NULL REFERENCES tx.tags(id) ON DELETE CASCADE,
    PRIMARY KEY (transaction_id, tag_id)
);

-- =====================================================================
-- Возврат в account: леджер баланса теперь ссылается на транзакции
-- =====================================================================
CREATE TABLE account.balance_ledger (
    id          BIGSERIAL PRIMARY KEY,
    account_id  UUID NOT NULL REFERENCES account.accounts(id) ON DELETE CASCADE,
    delta_minor BIGINT NOT NULL,
    reason      TEXT NOT NULL
                CHECK (reason IN ('transaction','adjustment','transfer')),
    ref_id      UUID REFERENCES tx.transactions(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_balance_ledger_account
    ON account.balance_ledger(account_id, created_at DESC);

-- =====================================================================
-- 7. BUDGET — бюджеты и лимиты
-- =====================================================================

CREATE TABLE budget.budgets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    period     TEXT NOT NULL DEFAULT 'monthly'
               CHECK (period IN ('monthly','weekly','custom')),
    starts_on  DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_budgets_org ON budget.budgets(org_id);

CREATE TABLE budget.budget_limits (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    budget_id             UUID NOT NULL REFERENCES budget.budgets(id) ON DELETE CASCADE,
    category_id           UUID NOT NULL REFERENCES tx.categories(id),
    limit_minor           BIGINT NOT NULL CHECK (limit_minor > 0),
    currency              CHAR(3) NOT NULL,
    include_subcategories BOOLEAN NOT NULL DEFAULT true,
    alert_pct             SMALLINT DEFAULT 80
);
CREATE UNIQUE INDEX uq_budget_category
    ON budget.budget_limits(budget_id, category_id);

CREATE TABLE budget.budget_progress (
    budget_limit_id UUID PRIMARY KEY REFERENCES budget.budget_limits(id) ON DELETE CASCADE,
    spent_minor     BIGINT NOT NULL DEFAULT 0,
    period_start    DATE NOT NULL,
    period_end      DATE NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =====================================================================
-- 8. INVEST — портфели, активы, сделки, котировки
-- =====================================================================

CREATE TABLE invest.portfolios (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_portfolios_org ON invest.portfolios(org_id);

CREATE TABLE invest.assets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    symbol     TEXT NOT NULL,
    asset_type TEXT NOT NULL
               CHECK (asset_type IN ('stock','crypto','deposit','bond')),
    currency   CHAR(3) NOT NULL,
    UNIQUE (symbol, asset_type)
);

CREATE TABLE invest.holdings (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    portfolio_id   UUID NOT NULL REFERENCES invest.portfolios(id) ON DELETE CASCADE,
    asset_id       UUID NOT NULL REFERENCES invest.assets(id),
    quantity       NUMERIC(28,10) NOT NULL DEFAULT 0,
    avg_cost_minor BIGINT NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_holding ON invest.holdings(portfolio_id, asset_id);

CREATE TABLE invest.trades (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    holding_id  UUID NOT NULL REFERENCES invest.holdings(id) ON DELETE CASCADE,
    side        TEXT NOT NULL CHECK (side IN ('buy','sell')),
    quantity    NUMERIC(28,10) NOT NULL CHECK (quantity > 0),
    price_minor BIGINT NOT NULL,
    executed_at TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_trades_holding
    ON invest.trades(holding_id, executed_at DESC);

CREATE TABLE invest.price_history (
    asset_id    UUID NOT NULL REFERENCES invest.assets(id) ON DELETE CASCADE,
    as_of       DATE NOT NULL,
    close_minor BIGINT NOT NULL,
    PRIMARY KEY (asset_id, as_of)
);

-- =====================================================================
-- 9. IMPORT — загрузка и разбор выписок
-- =====================================================================

CREATE TABLE import.import_jobs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    account_id    UUID NOT NULL REFERENCES account.accounts(id),
    file_name     TEXT NOT NULL,
    file_format   TEXT NOT NULL CHECK (file_format IN ('csv','ofx')),
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','parsing','mapping','completed','failed')),
    total_rows    INT,
    imported_rows INT,
    duplicate_rows INT,
    error_message TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ
);
CREATE INDEX idx_import_jobs_org ON import.import_jobs(org_id, created_at DESC);

-- Теперь, когда таблица создана, добавляем отложенный внешний ключ
ALTER TABLE tx.transactions
    ADD CONSTRAINT fk_tx_import_job
    FOREIGN KEY (import_job_id) REFERENCES import.import_jobs(id)
    ON DELETE SET NULL;

CREATE TABLE import.raw_rows (
    id                    BIGSERIAL PRIMARY KEY,
    import_job_id         UUID NOT NULL REFERENCES import.import_jobs(id) ON DELETE CASCADE,
    row_number            INT NOT NULL,
    raw_data              JSONB NOT NULL,
    dedup_hash            TEXT NOT NULL,
    status                TEXT NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending','mapped','duplicate','error')),
    mapped_transaction_id UUID REFERENCES tx.transactions(id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX uq_import_dedup
    ON import.raw_rows(import_job_id, dedup_hash);

-- =====================================================================
-- 10. ANALYTICS — read-модели для дашбордов
-- =====================================================================

CREATE TABLE analytics.daily_category_summary (
    org_id      UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    category_id UUID NOT NULL REFERENCES tx.categories(id),
    day         DATE NOT NULL,
    total_minor BIGINT NOT NULL DEFAULT 0,
    currency    CHAR(3) NOT NULL,
    PRIMARY KEY (org_id, category_id, day, currency)
);

CREATE TABLE analytics.net_worth_snapshots (
    org_id          UUID NOT NULL REFERENCES identity.organizations(id) ON DELETE CASCADE,
    as_of           DATE NOT NULL,
    net_worth_minor BIGINT NOT NULL,
    currency        CHAR(3) NOT NULL,
    PRIMARY KEY (org_id, as_of)
);

CREATE TABLE analytics.processed_events (
    event_id     UUID PRIMARY KEY,
    topic        TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =====================================================================
-- 11. ТРИГГЕРЫ — события и updated_at
-- =====================================================================

-- События -----------------------------------------------------------------
CREATE TRIGGER trg_users_events
    AFTER INSERT OR UPDATE OR DELETE ON identity.users
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('user','soft');

CREATE TRIGGER trg_accounts_events
    AFTER INSERT OR UPDATE OR DELETE ON account.accounts
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('account','soft');

CREATE TRIGGER trg_balance_ledger_events
    AFTER INSERT OR DELETE ON account.balance_ledger
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('balance_ledger','hard');

CREATE TRIGGER trg_categories_events
    AFTER INSERT OR UPDATE OR DELETE ON tx.categories
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('category','hard');

CREATE TRIGGER trg_transactions_events
    AFTER INSERT OR UPDATE OR DELETE ON tx.transactions
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('transaction','soft');

CREATE TRIGGER trg_tags_events
    AFTER INSERT OR UPDATE OR DELETE ON tx.tags
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('tag','hard');

CREATE TRIGGER trg_budgets_events
    AFTER INSERT OR UPDATE OR DELETE ON budget.budgets
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('budget','hard');

CREATE TRIGGER trg_budget_limits_events
    AFTER INSERT OR UPDATE OR DELETE ON budget.budget_limits
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('budget_limit','hard');

CREATE TRIGGER trg_portfolios_events
    AFTER INSERT OR UPDATE OR DELETE ON invest.portfolios
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('portfolio','hard');

CREATE TRIGGER trg_holdings_events
    AFTER INSERT OR UPDATE OR DELETE ON invest.holdings
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('holding','hard');

CREATE TRIGGER trg_trades_events
    AFTER INSERT OR UPDATE OR DELETE ON invest.trades
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('trade','hard');

CREATE TRIGGER trg_price_history_events
    AFTER INSERT OR DELETE ON invest.price_history
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('price_history','hard');

CREATE TRIGGER trg_import_jobs_events
    AFTER INSERT OR UPDATE OR DELETE ON import.import_jobs
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('import_job','hard');

CREATE TRIGGER trg_raw_rows_events
    AFTER INSERT OR UPDATE OR DELETE ON import.raw_rows
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('raw_row','hard');

CREATE TRIGGER trg_fx_rates_events
    AFTER INSERT OR DELETE ON fx.rates
    FOR EACH ROW EXECUTE FUNCTION app.event_notify('fx_rate','hard');

-- Автообновление updated_at ------------------------------------------------
CREATE TRIGGER trg_orgs_updated
    BEFORE UPDATE ON identity.organizations
    FOR EACH ROW EXECUTE FUNCTION app.set_updated_at();

CREATE TRIGGER trg_users_updated
    BEFORE UPDATE ON identity.users
    FOR EACH ROW EXECUTE FUNCTION app.set_updated_at();

CREATE TRIGGER trg_accounts_updated
    BEFORE UPDATE ON account.accounts
    FOR EACH ROW EXECUTE FUNCTION app.set_updated_at();

CREATE TRIGGER trg_transactions_updated
    BEFORE UPDATE ON tx.transactions
    FOR EACH ROW EXECUTE FUNCTION app.set_updated_at();

CREATE TRIGGER trg_budget_progress_updated
    BEFORE UPDATE ON budget.budget_progress
    FOR EACH ROW EXECUTE FUNCTION app.set_updated_at();

CREATE TRIGGER trg_holdings_updated
    BEFORE UPDATE ON invest.holdings
    FOR EACH ROW EXECUTE FUNCTION app.set_updated_at();

-- =====================================================================
-- 12. ПРЕДСТАВЛЕНИЯ для удобства
-- =====================================================================

-- Полный вид транзакции с именами счёта, категории и счёта назначения
CREATE VIEW tx.v_transactions_full AS
SELECT
    t.id,
    t.org_id,
    t.kind,
    t.amount_minor,
    t.currency,
    t.amount_in_account_currency_minor,
    t.occurred_at,
    t.description,
    t.source,
    t.created_at,
    a.id       AS account_id,
    a.name     AS account_name,
    a.currency AS account_currency,
    c.id       AS category_id,
    c.name     AS category_name,
    ta.id      AS to_account_id,
    ta.name    AS to_account_name
FROM tx.transactions t
LEFT JOIN account.accounts a  ON a.id  = t.account_id
LEFT JOIN tx.categories    c  ON c.id  = t.category_id
LEFT JOIN account.accounts ta ON ta.id = t.to_account_id
WHERE t.deleted_at IS NULL;

-- Активные счета с балансом
CREATE VIEW account.v_active_accounts AS
SELECT id, org_id, owner_user_id, name, type, currency, balance_minor
FROM account.accounts
WHERE deleted_at IS NULL AND is_archived = false;

-- =====================================================================
-- 13. НАЧАЛЬНЫЕ ДАННЫЕ
-- =====================================================================

-- Роли
INSERT INTO identity.roles(code) VALUES
    ('owner'), ('admin'), ('member'), ('viewer')
ON CONFLICT DO NOTHING;

-- Основные валюты
INSERT INTO fx.currencies(code, name, minor_unit) VALUES
    ('USD','US Dollar',2),
    ('EUR','Euro',2),
    ('RUB','Russian Ruble',2),
    ('GBP','British Pound',2),
    ('JPY','Japanese Yen',0),
    ('CHF','Swiss Franc',2),
    ('CNY','Chinese Yuan',2),
    ('KZT','Kazakhstani Tenge',2),
    ('UAH','Ukrainian Hryvnia',2),
    ('BTC','Bitcoin',8),
    ('ETH','Ethereum',18)
ON CONFLICT DO NOTHING;

-- Системные категории создаются вместе с организацией.
-- Пример вызова после регистрации пользователя:
--   1. Создать запись в identity.organizations
--   2. Вставить набор категорий с org_id и is_system = true

-- =====================================================================
-- 14. ВКЛЮЧЕНИЕ МУЛЬТИТЕНАНТНОСТИ (когда понадобится второй пользователь)
-- =====================================================================
-- Раскомментировать и добавить политики для каждой таблицы с org_id:
--
-- ALTER TABLE account.accounts ENABLE ROW LEVEL SECURITY;
-- CREATE POLICY accounts_tenant_isolation ON account.accounts
--     USING (org_id = current_setting('app.current_org')::UUID);
--
-- ALTER TABLE tx.transactions ENABLE ROW LEVEL SECURITY;
-- CREATE POLICY tx_tenant_isolation ON tx.transactions
--     USING (org_id = current_setting('app.current_org')::UUID);
--
-- ... повторить для всех таблиц с полем org_id ...
--
-- На уровне приложения перед каждым запросом:
--   SET LOCAL app.current_org = '<uuid>';
-- =====================================================================
-- >>> BEGIN db_schema_0002.sql >>>
-- =====================================================================
-- db_schema_fixes.sql — migration 0002
-- PostgreSQL 16+
-- =====================================================================

BEGIN;


-- ---------------------------------------------------------------------
-- 1. Event notifications:
--    - support tables without id / deleted_at;
--    - support composite keys via a JSON aggregate_id;
--    - reject missing or NULL key fields.
-- ---------------------------------------------------------------------

CREATE OR REPLACE FUNCTION app.event_notify()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    v_aggregate_type TEXT := TG_ARGV[0];
    v_soft           BOOLEAN := (TG_ARGV[1] = 'soft');
    v_new            JSONB;
    v_old            JSONB;
    v_row            JSONB;
    v_event          TEXT;
    v_id             TEXT;
    v_key            TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        v_old := to_jsonb(OLD);
        v_row := v_old;
    ELSE
        v_new := to_jsonb(NEW);
        v_row := v_new;

        IF TG_OP = 'UPDATE' THEN
            v_old := to_jsonb(OLD);
        END IF;
    END IF;

    -- Resolve aggregate ID.
    IF TG_NARGS >= 3 THEN
        -- Composite key, e.g. 'asset_id,as_of'.
        -- JSON avoids collisions caused by delimiter-based concatenation.
        FOR v_key IN
            SELECT btrim(k)
            FROM unnest(string_to_array(TG_ARGV[2], ','))
                 WITH ORDINALITY AS u(k, ord)
            ORDER BY ord
        LOOP
            IF v_key = ''
               OR NOT (v_row ? v_key)
               OR (v_row -> v_key) = 'null'::jsonb
            THEN
                RAISE EXCEPTION
                    'app.event_notify: missing or NULL key column "%" for %.%',
                    v_key, TG_TABLE_SCHEMA, TG_TABLE_NAME;
            END IF;
        END LOOP;

        SELECT (jsonb_object_agg(
                    btrim(k),
                    v_row -> btrim(k)
                    ORDER BY ord
                ))::text
          INTO v_id
          FROM unnest(string_to_array(TG_ARGV[2], ','))
               WITH ORDINALITY AS u(k, ord);

    ELSE
        v_id := v_row ->> 'id';
    END IF;

    IF v_id IS NULL OR v_id = '' THEN
        RAISE EXCEPTION
            'app.event_notify: aggregate ID is NULL or empty for %.%',
            TG_TABLE_SCHEMA, TG_TABLE_NAME;
    END IF;

    -- Determine event type.
    IF TG_OP = 'DELETE' THEN
        v_event := v_aggregate_type || '.deleted';

    ELSIF TG_OP = 'INSERT' THEN
        v_event := v_aggregate_type || '.created';

    ELSIF v_soft
          AND (v_new ->> 'deleted_at') IS NOT NULL
          AND (v_old ->> 'deleted_at') IS NULL
    THEN
        v_event := v_aggregate_type || '.deleted';

    ELSE
        v_event := v_aggregate_type || '.updated';
    END IF;

    PERFORM app.publish_event(
        v_aggregate_type,
        v_id,
        v_event,
        v_row
    );

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    ELSE
        RETURN NEW;
    END IF;
END;
$$;


-- Recreate the price history trigger.
DROP TRIGGER IF EXISTS trg_price_history_events
    ON invest.price_history;

CREATE TRIGGER trg_price_history_events
    AFTER INSERT OR DELETE ON invest.price_history
    FOR EACH ROW
    EXECUTE FUNCTION app.event_notify(
        'price_history',
        'hard',
        'asset_id,as_of'
    );


-- ---------------------------------------------------------------------
-- 2. Category uniqueness:
--    NULL parent IDs are treated as equal for uniqueness.
--    Existing duplicates must be resolved before this step.
-- ---------------------------------------------------------------------

ALTER TABLE tx.categories
    DROP CONSTRAINT categories_org_id_parent_id_name_key;

ALTER TABLE tx.categories
    ADD CONSTRAINT categories_org_parent_name_key
    UNIQUE NULLS NOT DISTINCT (org_id, parent_id, name);


-- ---------------------------------------------------------------------
-- 3. Currency foreign keys.
--    NOT VALID avoids scanning all existing rows when adding each FK.
--    Existing rows are checked in the validation phase below.
-- ---------------------------------------------------------------------

ALTER TABLE identity.organizations
    ADD CONSTRAINT fk_org_base_currency
    FOREIGN KEY (base_currency)
    REFERENCES fx.currencies(code)
    NOT VALID;

ALTER TABLE account.accounts
    ADD CONSTRAINT fk_account_currency
    FOREIGN KEY (currency)
    REFERENCES fx.currencies(code)
    NOT VALID;

ALTER TABLE tx.transactions
    ADD CONSTRAINT fk_tx_currency
    FOREIGN KEY (currency)
    REFERENCES fx.currencies(code)
    NOT VALID;

ALTER TABLE budget.budget_limits
    ADD CONSTRAINT fk_limit_currency
    FOREIGN KEY (currency)
    REFERENCES fx.currencies(code)
    NOT VALID;

ALTER TABLE invest.assets
    ADD CONSTRAINT fk_asset_currency
    FOREIGN KEY (currency)
    REFERENCES fx.currencies(code)
    NOT VALID;

ALTER TABLE analytics.daily_category_summary
    ADD CONSTRAINT fk_dcs_currency
    FOREIGN KEY (currency)
    REFERENCES fx.currencies(code)
    NOT VALID;

ALTER TABLE analytics.net_worth_snapshots
    ADD CONSTRAINT fk_nws_currency
    FOREIGN KEY (currency)
    REFERENCES fx.currencies(code)
    NOT VALID;


-- Validate existing rows. Migration fails if any unknown currency exists.
ALTER TABLE identity.organizations
    VALIDATE CONSTRAINT fk_org_base_currency;

ALTER TABLE account.accounts
    VALIDATE CONSTRAINT fk_account_currency;

ALTER TABLE tx.transactions
    VALIDATE CONSTRAINT fk_tx_currency;

ALTER TABLE budget.budget_limits
    VALIDATE CONSTRAINT fk_limit_currency;

ALTER TABLE invest.assets
    VALIDATE CONSTRAINT fk_asset_currency;

ALTER TABLE analytics.daily_category_summary
    VALIDATE CONSTRAINT fk_dcs_currency;

ALTER TABLE analytics.net_worth_snapshots
    VALIDATE CONSTRAINT fk_nws_currency;

COMMIT;
-- >>> BEGIN db_schema_0003.sql >>>
-- =====================================================================
-- db_schema_fixes_2.sql — migration 0003
-- PostgreSQL 16+
-- =====================================================================
-- Что делает эта миграция:
--
--   1. Добавляет UUID-идентификатор `uid` в app.events — ключ для
--      идемпотентной обработки воркерами (связь с
--      analytics.processed_events.event_id).
--   2. Обновляет app.publish_event(): теперь она возвращает `uid` в
--      NOTIFY-уведомлении, чтобы consumer мог проверить/записать его
--      в processed_events.
--   3. Добавляет индекс (aggregate_type, aggregate_id) для быстрого
--      поиска при обработке обрезанных (truncated) уведомлений.
--   4. Ужесточает app.event_notify(): таблица, помеченная как 'soft',
--      обязана иметь колонку deleted_at — иначе триггер упадёт сразу.
--   5. Добавляет CHECK (quantity >= 0) на invest.holdings — защита от
--      ухода позиции в «шорт» на уровне БД.
--
-- ---------------------------------------------------------------------
-- ФОРМАТ ПОЛЯ app.events.aggregate_id (контракт для потребителей):
--
--   • Для таблиц с одним ключом (id UUID или BIGSERIAL) — скаляр:
--         '3f7a1c2e-…'            (UUID)
--         '42'                     (BIGSERIAL → текст)
--
--   • Для таблиц с составным ключом — JSON-объект:
--         '{"asset_id":"3f7a…","as_of":"2026-10-12"}'
--
--   Алгоритм разбора в Go-воркере:
--     1. Попробовать распарсить как UUID.
--     2. При неудаче — как JSON-объект (начинается с '{').
--     3. Иначе трактовать как скалярный текст (например, BIGINT).
-- =====================================================================

BEGIN;

-- ---------------------------------------------------------------------
-- 1. Добавляем `uid` в app.events.
--
--    Примечание по производительности: DEFAULT gen_random_uuid() —
--    volatile-функция, поэтому при добавлении колонки на большой
--    существующей таблице значения будут вычислены для каждой строки.
--    На пустой/молодой БД это мгновенно.
-- ---------------------------------------------------------------------
ALTER TABLE app.events
    ADD COLUMN IF NOT EXISTS uid UUID NOT NULL DEFAULT gen_random_uuid();

-- Уникальный индекс для быстрого поиска по `uid`.
CREATE UNIQUE INDEX IF NOT EXISTS uq_app_events_uid
    ON app.events(uid);

-- ---------------------------------------------------------------------
-- 2. Обновляем app.publish_event(): включаем `uid` в уведомление.
-- ---------------------------------------------------------------------
CREATE OR REPLACE FUNCTION app.publish_event(
    p_aggregate_type TEXT,
    p_aggregate_id   TEXT,
    p_event_type     TEXT,
    p_payload        JSONB
) RETURNS void AS $$
DECLARE
    v_uid          UUID;
    v_notification JSONB;
BEGIN
    INSERT INTO app.events(aggregate_type, aggregate_id, event_type, payload)
    VALUES (p_aggregate_type, p_aggregate_id, p_event_type, p_payload)
    RETURNING uid INTO v_uid;

    v_notification := jsonb_build_object(
        'uid',            v_uid,
        'event_type',     p_event_type,
        'aggregate_type', p_aggregate_type,
        'aggregate_id',   p_aggregate_id,
        'payload',        p_payload
    );

    -- Обрезаем до безопасного размера, если строка слишком длинная.
    IF octet_length(v_notification::text) > 7000 THEN
        v_notification := jsonb_build_object(
            'uid',            v_uid,
            'event_type',     p_event_type,
            'aggregate_type', p_aggregate_type,
            'aggregate_id',   p_aggregate_id,
            'payload',        '{}'::jsonb,
            'truncated',      true
        );
    END IF;

    PERFORM pg_notify('app_events', v_notification::text);
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------
-- 3. Индекс для быстрого поиска по типу агрегата и идентификатору
--    (нужен при обработке обрезанных уведомлений и пересборке).
-- ---------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS idx_app_events_aggregate
    ON app.events(aggregate_type, aggregate_id);

-- ---------------------------------------------------------------------
-- 4. Обновляем app.event_notify():
--    • валидация: 'soft'-таблица обязана иметь deleted_at;
--    • сохранена логика составных ключей из миграции 0002.
--
--    Функция заменяется на месте — все существующие триггеры
--    автоматически используют новую версию.
-- ---------------------------------------------------------------------
CREATE OR REPLACE FUNCTION app.event_notify()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    v_aggregate_type TEXT := TG_ARGV[0];
    v_soft           BOOLEAN := (TG_ARGV[1] = 'soft');
    v_new            JSONB;
    v_old            JSONB;
    v_row            JSONB;
    v_event          TEXT;
    v_id             TEXT;
    v_key            TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        v_old := to_jsonb(OLD);
        v_row := v_old;
    ELSE
        v_new := to_jsonb(NEW);
        v_row := v_new;
        IF TG_OP = 'UPDATE' THEN
            v_old := to_jsonb(OLD);
        END IF;
    END IF;

    -- Валидация конфигурации: 'soft' без deleted_at — ошибка.
    IF v_soft AND NOT (v_row ? 'deleted_at') THEN
        RAISE EXCEPTION
            'app.event_notify: table %.% is marked soft but has no deleted_at column',
            TG_TABLE_SCHEMA, TG_TABLE_NAME;
    END IF;

    -- Разрешаем aggregate ID.
    IF TG_NARGS >= 3 THEN
        -- Составной ключ, например 'asset_id,as_of'.
        -- JSON исключает коллизии, возможные при конкатенации.
        FOR v_key IN
            SELECT btrim(k)
            FROM unnest(string_to_array(TG_ARGV[2], ','))
                 WITH ORDINALITY AS u(k, ord)
            ORDER BY ord
        LOOP
            IF v_key = ''
               OR NOT (v_row ? v_key)
               OR (v_row -> v_key) = 'null'::jsonb
            THEN
                RAISE EXCEPTION
                    'app.event_notify: missing or NULL key column "%" for %.%',
                    v_key, TG_TABLE_SCHEMA, TG_TABLE_NAME;
            END IF;
        END LOOP;

        SELECT (jsonb_object_agg(
                    btrim(k),
                    v_row -> btrim(k)
                    ORDER BY ord
                ))::text
          INTO v_id
          FROM unnest(string_to_array(TG_ARGV[2], ','))
               WITH ORDINALITY AS u(k, ord);
    ELSE
        v_id := v_row ->> 'id';
    END IF;

    IF v_id IS NULL OR v_id = '' THEN
        RAISE EXCEPTION
            'app.event_notify: aggregate ID is NULL or empty for %.%',
            TG_TABLE_SCHEMA, TG_TABLE_NAME;
    END IF;

    -- Определяем тип события.
    IF TG_OP = 'DELETE' THEN
        v_event := v_aggregate_type || '.deleted';
    ELSIF TG_OP = 'INSERT' THEN
        v_event := v_aggregate_type || '.created';
    ELSIF v_soft
          AND (v_new ->> 'deleted_at') IS NOT NULL
          AND (v_old ->> 'deleted_at') IS NULL
    THEN
        v_event := v_aggregate_type || '.deleted';
    ELSE
        v_event := v_aggregate_type || '.updated';
    END IF;

    PERFORM app.publish_event(
        v_aggregate_type,
        v_id,
        v_event,
        v_row
    );

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    ELSE
        RETURN NEW;
    END IF;
END;
$$;

-- ---------------------------------------------------------------------
-- 5. Защита от отрицательного количества в инвест-позициях.
--
--    Используем NOT VALID + VALIDATE, чтобы:
--      • не сканировать таблицу при добавлении ограничения;
--      • проверить существующие строки отдельным шагом.
--    Если в holdings есть quantity < 0, миграция упадёт и откатится.
-- ---------------------------------------------------------------------
ALTER TABLE invest.holdings
    ADD CONSTRAINT chk_holding_qty_nonneg
    CHECK (quantity >= 0)
    NOT VALID;

ALTER TABLE invest.holdings
    VALIDATE CONSTRAINT chk_holding_qty_nonneg;

COMMIT;

-- =====================================================================
-- СВЯЗЬ С GO-ВОРКЕРОМ (на уровне приложения, не в БД):
--
--   1. При получении уведомления воркер извлекает `uid`.
--
--   2. Перед обработкой проверяет идемпотентность:
--        SELECT 1
--          FROM analytics.processed_events
--         WHERE event_id = $uid;
--      Если запись есть — событие уже обработано, пропускаем.
--
--   3. После успешной обработки фиксирует:
--        INSERT INTO analytics.processed_events(event_id, topic)
--        VALUES ($uid, $topic)
--        ON CONFLICT (event_id) DO NOTHING;
--
--   Примечание: намеренно НЕ создаём FOREIGN KEY
--   processed_events.event_id → app.events.uid, чтобы будущая
--   ротация/архивация журнала событий (п. 8 чек-листа) не ломала
--   таблицу обработанных событий.
-- =====================================================================
-- <<< END merged baseline <<<
