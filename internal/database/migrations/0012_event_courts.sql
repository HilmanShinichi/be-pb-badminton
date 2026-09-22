-- Configurable court count per match night. 0 = unlimited.
ALTER TABLE match_events
    ADD COLUMN IF NOT EXISTS court_count INTEGER NOT NULL DEFAULT 0;
