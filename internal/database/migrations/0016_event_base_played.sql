-- Starting play count per event: displayed totals add this base.
ALTER TABLE match_events
    ADD COLUMN IF NOT EXISTS base_played INTEGER NOT NULL DEFAULT 0;
