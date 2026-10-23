-- 000010 — Remove budget.budget_plans table and trigger
DROP TRIGGER IF EXISTS trg_budget_plans_updated_at ON budget.budget_plans;
DROP FUNCTION IF EXISTS budget.budget_plans_updated_at();

DROP TABLE IF EXISTS budget.budget_plans CASCADE;