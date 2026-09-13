CREATE OR REPLACE FUNCTION prune_product_when_accounts_gone()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM trading_accounts WHERE product_id = OLD.product_id
    ) THEN
        DELETE FROM products WHERE id = OLD.product_id;
    END IF;
    RETURN OLD;
END;
$$;

DROP TRIGGER IF EXISTS trading_accounts_prune_orphan_product ON trading_accounts;
CREATE TRIGGER trading_accounts_prune_orphan_product
AFTER DELETE ON trading_accounts
FOR EACH ROW EXECUTE FUNCTION prune_product_when_accounts_gone();

DELETE FROM products p
WHERE NOT EXISTS (
    SELECT 1 FROM trading_accounts a WHERE a.product_id = p.id
);
