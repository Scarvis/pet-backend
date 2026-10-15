CREATE OR REPLACE VIEW account.v_active_accounts AS
SELECT id, org_id, owner_user_id, name, type, currency, balance_minor
FROM account.accounts
WHERE deleted_at IS NULL AND is_archived = false;

ALTER TABLE account.accounts DROP COLUMN IF EXISTS is_hidden;
