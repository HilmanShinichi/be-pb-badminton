-- Per-event referee switch. Existing events keep referees (default TRUE);
-- turning it off stops all automatic referee assignment so the club can
-- focus on players first.
ALTER TABLE match_events ADD COLUMN IF NOT EXISTS use_referee BOOLEAN NOT NULL DEFAULT TRUE;
