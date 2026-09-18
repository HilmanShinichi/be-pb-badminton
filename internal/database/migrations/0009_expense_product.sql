-- Link stock purchases to their cash expense so the dashboard can split
-- Daily vs Period cash flow by product purpose when no session is linked.
ALTER TABLE expenses
    ADD COLUMN IF NOT EXISTS product_id UUID REFERENCES shuttlecock_products(id);
CREATE INDEX IF NOT EXISTS idx_expenses_product ON expenses (product_id);
