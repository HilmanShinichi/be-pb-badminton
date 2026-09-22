-- Public live view controls per match night.
ALTER TABLE match_events
    ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS show_grades BOOLEAN NOT NULL DEFAULT false;
