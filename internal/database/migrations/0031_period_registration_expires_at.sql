ALTER TABLE membership_periods
    ADD COLUMN IF NOT EXISTS registration_expires_at TIMESTAMPTZ;
