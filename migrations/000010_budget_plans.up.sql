-- 000010 — budget.budget_plans: explicit plan values per (period_type, period_start, row)
-- Coexists with existing budget.budgets / budget_limits / budget_progress — untouched.

-- Create the budget_plans table
CREATE TABLE budget.budget_plans (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id              UUID NOT NULL REFERENCES identity.organizations(id),
    period_type         TEXT NOT NULL CHECK (period_type IN ('month', 'quarter', 'year')),
    period_start        DATE NOT NULL,
    ref_type            TEXT NOT NULL CHECK (ref_type IN ('category', 'account')),
    category_id         UUID REFERENCES tx.categories(id),
    account_id          UUID REFERENCES account.accounts(id),
    amount_minor        BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency            CHAR(3) NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Exactly one of category_id / account_id must be set, matching ref_type.
    CHECK ((ref_type = 'category') = (category_id IS NOT NULL)),
    CHECK ((ref_type = 'account')  = (account_id  IS NOT NULL)),
    -- Org-scoped unique plan per (period_type, period_start, ref_type, ref_id).
    -- NULLS NOT DISTINCT means two NULL ref_ids are considered equal (only one
    -- ref_type is ever set per row due to the CHECK above).
    UNIQUE NULLS NOT DISTINCT (org_id, period_type, period_start, category_id, account_id)
);

-- Add comment
COMMENT ON TABLE budget.budget_plans IS 'Explicit budget plan values per org, period, and reference (category or account)';

-- Add updated_at trigger
CREATE OR REPLACE FUNCTION budget.budget_plans_updated_at()
RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_budget_plans_updated_at
    BEFORE UPDATE ON budget.budget_plans
    FOR EACH ROW EXECUTE FUNCTION budget.budget_plans_updated_at();

-- Grant permissions (RLS will be managed by Hasura metadata)
GRANT ALL ON budget.budget_plans TO postgres;
GRANT SELECT, INSERT, UPDATE, DELETE ON budget.budget_plans TO public;