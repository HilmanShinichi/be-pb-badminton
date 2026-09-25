-- Planned rounds per event. 0 means no limit; the frontend also uses it as the
-- scale for per-player played and refereed bars (one match per player a round).
ALTER TABLE match_events
    ADD COLUMN IF NOT EXISTS max_rounds INTEGER NOT NULL DEFAULT 0;
