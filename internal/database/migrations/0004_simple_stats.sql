-- Simple recap per player for daily/event sessions.
-- Lets admins record "nama, total main, total kok" without composing 2v2 matches.
CREATE TABLE IF NOT EXISTS session_player_stats (
    session_id UUID NOT NULL REFERENCES mabar_sessions(id) ON DELETE CASCADE,
    player_id UUID NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    play_count INT NOT NULL DEFAULT 0 CHECK (play_count >= 0),
    shuttlecock_used INT NOT NULL DEFAULT 0 CHECK (shuttlecock_used >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, player_id)
);
CREATE INDEX IF NOT EXISTS idx_session_player_stats_session ON session_player_stats (session_id);
