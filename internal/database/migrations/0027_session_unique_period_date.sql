-- Prevent multiple sessions in the same membership period sharing a date.
-- Standalone daily/event sessions (period_id IS NULL) are exempt: they have
-- no period calendar to clash with.
CREATE UNIQUE INDEX IF NOT EXISTS uq_mabar_sessions_period_date
    ON mabar_sessions (period_id, date)
    WHERE period_id IS NOT NULL;
