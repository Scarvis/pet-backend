-- Add client-side visibility flag to accounts. Independent from archiving
-- (deleted_at): hidden accounts stay active, keep their balances and count
-- toward net worth; clients filter them out of default lists.
ALTER TABLE account.accounts
    ADD COLUMN IF NOT EXISTS is_hidden BOOLEAN NOT NULL DEFAULT false;

-- Expose the flag in the active-accounts view.
CREATE OR REPLACE VIEW account.v_active_accounts AS
SELECT id, org_id, owner_user_id, name, type, currency, balance_minor, is_hidden
FROM account.accounts
WHERE deleted_at IS NULL AND is_archived = false;
