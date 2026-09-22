-- Play timer + shuttlecock counter per generated doubles match.
ALTER TABLE generated_matches
    ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ended_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS shuttlecock_used INTEGER NOT NULL DEFAULT 0;
