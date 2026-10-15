-- Russian system categories backfill (new orgs are seeded by
-- internal/auth insertSystemCategories: roots Доходы/Расходы + 25 children,
-- 27 rows total). Existing orgs keep their old English roots; children are
-- attached to the existing root of the same kind.
--
-- Then enforces the 100-categories-per-org cap via trigger (backfill runs
-- first so it cannot trip on its own inserts).

DO $$
DECLARE
    r_org RECORD;
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
    FOR r_org IN SELECT id FROM identity.organizations LOOP
        SELECT id INTO v_income_root
        FROM tx.categories
        WHERE org_id = r_org.id AND kind = 'income' AND parent_id IS NULL AND is_system
        ORDER BY name LIMIT 1;
        IF v_income_root IS NULL THEN
            INSERT INTO tx.categories (org_id, parent_id, name, kind, is_system)
            VALUES (r_org.id, NULL, 'Доходы', 'income', true)
            RETURNING id INTO v_income_root;
        END IF;

        SELECT id INTO v_expense_root
        FROM tx.categories
        WHERE org_id = r_org.id AND kind = 'expense' AND parent_id IS NULL AND is_system
        ORDER BY name LIMIT 1;
        IF v_expense_root IS NULL THEN
            INSERT INTO tx.categories (org_id, parent_id, name, kind, is_system)
            VALUES (r_org.id, NULL, 'Расходы', 'expense', true)
            RETURNING id INTO v_expense_root;
        END IF;

        FOREACH v_child IN ARRAY v_income_children LOOP
            INSERT INTO tx.categories (org_id, parent_id, name, kind, is_system)
            VALUES (r_org.id, v_income_root, v_child, 'income', true)
            ON CONFLICT (org_id, parent_id, name) DO NOTHING;
        END LOOP;

        FOREACH v_child IN ARRAY v_expense_children LOOP
            INSERT INTO tx.categories (org_id, parent_id, name, kind, is_system)
            VALUES (r_org.id, v_expense_root, v_child, 'expense', true)
            ON CONFLICT (org_id, parent_id, name) DO NOTHING;
        END LOOP;

        v_income_root := NULL;
        v_expense_root := NULL;
    END LOOP;
END $$;

-- Max 100 categories per org (roots + children + user categories).
CREATE OR REPLACE FUNCTION tx.enforce_category_limit()
RETURNS trigger AS $$
BEGIN
    IF (SELECT count(*) FROM tx.categories WHERE org_id = NEW.org_id) >= 100 THEN
        RAISE EXCEPTION 'category limit (100 per organization) exceeded' USING ERRCODE = 'P0001';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_categories_limit ON tx.categories;
CREATE TRIGGER trg_categories_limit
    BEFORE INSERT ON tx.categories
    FOR EACH ROW EXECUTE FUNCTION tx.enforce_category_limit();
