-- Client-side visibility flag for categories, mirroring account.accounts.is_hidden
-- (000005). Hidden categories stay active and keep their transactions/budgets;
-- clients filter them out of default lists.
ALTER TABLE tx.categories
    ADD COLUMN IF NOT EXISTS is_hidden BOOLEAN NOT NULL DEFAULT false;
