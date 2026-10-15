-- 000006 rollback: drop the 100-per-org cap. Seeded Russian categories are
-- left in place (removing them would NULL out transactions referencing them
-- via ON DELETE SET NULL); delete unreferenced ones manually if needed.
DROP TRIGGER IF EXISTS trg_categories_limit ON tx.categories;
DROP FUNCTION IF EXISTS tx.enforce_category_limit();
