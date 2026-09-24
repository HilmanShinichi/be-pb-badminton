-- Remember which open-play session an event pool was taken from so late
-- arrivals can be offered from that session instead of all players.
ALTER TABLE match_events
    ADD COLUMN IF NOT EXISTS source_session_id UUID REFERENCES mabar_sessions(id) ON DELETE SET NULL;
