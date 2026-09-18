ALTER TABLE membership_periods
    ADD COLUMN IF NOT EXISTS session_weekdays INT[] NOT NULL DEFAULT '{}';

ALTER TABLE membership_periods DROP COLUMN IF EXISTS weekday;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'membership_periods_session_weekdays_check'
    ) THEN
        ALTER TABLE membership_periods
            ADD CONSTRAINT membership_periods_session_weekdays_check
            CHECK (session_weekdays <@ ARRAY[0, 1, 2, 3, 4, 5, 6]);
    END IF;
END $$;
