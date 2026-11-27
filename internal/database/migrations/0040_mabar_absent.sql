CREATE TABLE absent (
    session_id UUID NOT NULL REFERENCES mabar_sessions(id) ON DELETE CASCADE,
    player_id UUID NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    marked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, player_id)
);
