-- Add customizable registration notes / S&K per period
ALTER TABLE membership_periods
    ADD COLUMN IF NOT EXISTS registration_notes TEXT;
