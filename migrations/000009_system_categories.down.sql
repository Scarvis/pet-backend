-- 000009 rollback: restore tx.categories.is_system and drop the catalog.
-- Categories matching a system template by (parent name, name, kind) are
-- flagged is_system = true again (best-effort: user rows that happen to
-- share a template name are flagged too); everything else stays false.

ALTER TABLE tx.categories
    ADD COLUMN IF NOT EXISTS is_system BOOLEAN NOT NULL DEFAULT false;

-- Roots matching a system root template.
UPDATE tx.categories c SET is_system = true
FROM tx.system_categories s
WHERE c.parent_id IS NULL
  AND s.parent_id IS NULL
  AND c.name = s.name AND c.kind = s.kind;

-- Children matching a system child template under a matching root.
UPDATE tx.categories c SET is_system = true
FROM tx.categories p
JOIN tx.system_categories sc ON sc.name = c.name AND sc.kind = c.kind
JOIN tx.system_categories sp ON sp.id = sc.parent_id
WHERE c.parent_id = p.id
  AND p.parent_id IS NULL
  AND sp.parent_id IS NULL
  AND p.name = sp.name AND p.kind = sp.kind;

DROP TABLE IF EXISTS tx.system_categories;
