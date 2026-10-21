-- 000008: account type catalog + opening balance.
--
-- - Creates account.accounts_type(code PK) seeded with the 9 supported
--   account types.
-- - Migrates the legacy 'card' type to 'debit card', drops the old
--   CHECK constraint, and replaces it with a FK to the catalog so only
--   catalogued types are accepted.
-- - Adds accounts.initial_amount (opening balance, minor units, default 0)
--   and exposes it in account.v_active_accounts.

-- Catalog of allowed account types.
CREATE TABLE IF NOT EXISTS account.accounts_type (
    code       TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO account.accounts_type (code) VALUES
    ('debit card'),
    ('cash'),
    ('deposit'),
    ('broker'),
    ('credit card'),
    ('credit'),
    ('debt'),
    ('crypto'),
    ('saving')
ON CONFLICT (code) DO NOTHING;

-- Drop the old pseudo-enum CHECK (auto-named <table>_<column>_check) BEFORE
-- rewriting legacy values: the old CHECK set still lists 'card' and would
-- reject 'debit card' on populated tables.
ALTER TABLE account.accounts DROP CONSTRAINT IF EXISTS accounts_type_check;

-- Legacy value 'card' (pre-000008 CHECK set) becomes 'debit card' so the
-- FK below validates on existing data.
UPDATE account.accounts SET type = 'debit card' WHERE type = 'card';

-- Opening balance in minor units. New accounts start with
-- balance_minor = initial_amount (see internal/accounts Create); existing
-- rows keep their balance and get initial_amount = 0 (unknown opening).
ALTER TABLE account.accounts
    ADD COLUMN IF NOT EXISTS initial_amount BIGINT NOT NULL DEFAULT 0;

-- Only catalogued types are accepted from now on. Guarded so re-runs on a
-- partially applied database skip a constraint that already exists.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'fk_accounts_type'
          AND conrelid = 'account.accounts'::regclass
    ) THEN
        ALTER TABLE account.accounts
            ADD CONSTRAINT fk_accounts_type
            FOREIGN KEY (type) REFERENCES account.accounts_type (code);
    END IF;
END $$;

-- Expose the new column in the active-accounts view. On databases where the
-- view already exists with is_hidden as the last column, CREATE OR REPLACE
-- cannot insert initial_amount before it, so recreate it.
DROP VIEW IF EXISTS account.v_active_accounts;
CREATE VIEW account.v_active_accounts AS
SELECT id, org_id, owner_user_id, name, type, currency, balance_minor,
       initial_amount, is_hidden
FROM account.accounts
WHERE deleted_at IS NULL AND is_archived = false;
