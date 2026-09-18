ALTER TABLE membership_periods
    ADD COLUMN IF NOT EXISTS duration_weeks INT,
    ADD COLUMN IF NOT EXISTS weekday INT,
    ADD COLUMN IF NOT EXISTS default_start_time TIME,
    ADD COLUMN IF NOT EXISTS default_end_time TIME;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'membership_periods_weekday_check'
    ) THEN
        ALTER TABLE membership_periods
            ADD CONSTRAINT membership_periods_weekday_check
            CHECK (weekday IS NULL OR (weekday >= 0 AND weekday <= 6));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'membership_periods_duration_check'
    ) THEN
        ALTER TABLE membership_periods
            ADD CONSTRAINT membership_periods_duration_check
            CHECK (duration_weeks IS NULL OR duration_weeks >= 1);
    END IF;
END $$;
