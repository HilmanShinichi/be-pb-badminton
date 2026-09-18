-- Period cost settings so kalkulasi tidak 0 sebelum transaksi manual.
ALTER TABLE membership_periods
    ADD COLUMN IF NOT EXISTS venue_cost_total BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS shuttle_pack_price BIGINT NOT NULL DEFAULT 125000,
    ADD COLUMN IF NOT EXISTS shuttle_units_per_pack INT NOT NULL DEFAULT 12,
    ADD COLUMN IF NOT EXISTS shuttle_per_session INT NOT NULL DEFAULT 24;
