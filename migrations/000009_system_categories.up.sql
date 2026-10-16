-- 000009: global tx.system_categories catalog replacing per-org is_system rows.
--
-- - Creates tx.system_categories (org-free template tree, same shape as
--   tx.categories minus org_id/is_system).
-- - Moves existing system rows (is_system = true) from tx.categories into
--   it, deduplicated by (parent name, name, kind): roots first, then
--   children resolved through the already-moved roots.
-- - Ensures the default Russian set (roots Доходы/Расходы + 25 children,
--   same as the backend seeder) exists for fresh databases.
-- - Drops tx.categories.is_system. From now on Register seeds a new org by
--   copying tx.system_categories (see internal/auth insertSystemCategories).

-- Org-free template tree. Uniqueness mirrors tx.categories
-- (UNIQUE NULLS NOT DISTINCT (org_id, parent_id, name)) minus org_id.
CREATE TABLE IF NOT EXISTS tx.system_categories (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id  UUID REFERENCES tx.system_categories (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('income', 'expense')),
    icon       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (parent_id, name)
);

-- Move existing system roots (deduplicated by name + kind).
INSERT INTO tx.system_categories (parent_id, name, kind, icon)
SELECT NULL, name, kind, MIN(icon)
FROM tx.categories
WHERE is_system AND parent_id IS NULL
GROUP BY name, kind
ON CONFLICT DO NOTHING;

-- Move existing system children, resolved through the moved roots
-- (matched by root name + kind, since per-org parent UUIDs don't transfer).
INSERT INTO tx.system_categories (parent_id, name, kind, icon)
SELECT sp.id, c.name, c.kind, MIN(c.icon)
FROM tx.categories c
JOIN tx.categories p ON p.id = c.parent_id
JOIN tx.system_categories sp
    ON sp.parent_id IS NULL AND sp.name = p.name AND sp.kind = p.kind
WHERE c.is_system AND c.parent_id IS NOT NULL
GROUP BY sp.id, c.name, c.kind
ON CONFLICT DO NOTHING;

-- Ensure the default Russian set exists (fresh databases have no
-- tx.categories rows to move). Idempotent: re-runs and pre-moved rows
-- hit ON CONFLICT DO NOTHING.
DO $$
DECLARE
    v_income_root UUID;
    v_expense_root UUID;
    v_income_children TEXT[] := ARRAY[
        'Зарплата', 'Фриланс', 'Бизнес', 'Инвестиции', 'Аренда', 'Подарки', 'Прочее'
    ];
    v_expense_children TEXT[] := ARRAY[
        'Продукты', 'Кафе и рестораны', 'Транспорт', 'Жильё',
        'Коммунальные услуги', 'Связь и интернет', 'Здоровье',
        'Одежда и обувь', 'Образование', 'Развлечения', 'Спорт',
        'Дети', 'Питомцы', 'Путешествия', 'Красота', 'Подписки',
        'Налоги', 'Прочее'
    ];
    v_child TEXT;
BEGIN
    INSERT INTO tx.system_categories (parent_id, name, kind)
    VALUES (NULL, 'Доходы', 'income')
    ON CONFLICT DO NOTHING;
    SELECT id INTO v_income_root FROM tx.system_categories
    WHERE parent_id IS NULL AND name = 'Доходы' AND kind = 'income';

    INSERT INTO tx.system_categories (parent_id, name, kind)
    VALUES (NULL, 'Расходы', 'expense')
    ON CONFLICT DO NOTHING;
    SELECT id INTO v_expense_root FROM tx.system_categories
    WHERE parent_id IS NULL AND name = 'Расходы' AND kind = 'expense';

    FOREACH v_child IN ARRAY v_income_children LOOP
        INSERT INTO tx.system_categories (parent_id, name, kind)
        VALUES (v_income_root, v_child, 'income')
        ON CONFLICT DO NOTHING;
    END LOOP;

    FOREACH v_child IN ARRAY v_expense_children LOOP
        INSERT INTO tx.system_categories (parent_id, name, kind)
        VALUES (v_expense_root, v_child, 'expense')
        ON CONFLICT DO NOTHING;
    END LOOP;
END $$;

-- Per-org rows are now all user rows; the template lives in system_categories.
ALTER TABLE tx.categories DROP COLUMN IF EXISTS is_system;
