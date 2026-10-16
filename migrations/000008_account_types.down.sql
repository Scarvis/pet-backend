-- 000008 rollback: drop the FK + opening balance, restore the legacy
-- CHECK set. Rows using post-000008 types are mapped back ('debit card' ->
-- 'card', anything else unknown -> 'cash') so the restored CHECK validates.
-- The accounts_type catalog is left in place when still referenced;
-- drop it manually once no account uses it.

-- Restore legacy value for the one renamed type; map every other new type
-- to 'cash' (lossy but rollback-only; forward data stays intact otherwise).
UPDATE account.accounts SET type = 'card' WHERE type = 'debit card';
UPDATE account.accounts SET type = 'cash'
WHERE type NOT IN ('cash', 'card', 'deposit', 'debt');

ALTER TABLE account.accounts DROP CONSTRAINT IF EXISTS fk_accounts_type;

ALTER TABLE account.accounts
    ADD CONSTRAINT accounts_type_check
    CHECK (type IN ('cash', 'card', 'deposit', 'debt')) NOT VALID;
ALTER TABLE account.accounts VALIDATE CONSTRAINT accounts_type_check;

CREATE OR REPLACE VIEW account.v_active_accounts AS
SELECT id, org_id, owner_user_id, name, type, currency, balance_minor, is_hidden
FROM account.accounts
WHERE deleted_at IS NULL AND is_archived = false;

ALTER TABLE account.accounts DROP COLUMN IF EXISTS initial_amount;
