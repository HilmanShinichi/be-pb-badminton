-- Personal play caps per event pool member. NULL/absent row means "follow
-- the event limit" (max_rounds, 0 = unlimited). A higher personal cap (e.g.
-- the member who paid the courts) lets them keep playing past the event max.
CREATE TABLE IF NOT EXISTS match_player_caps (
    event_id UUID NOT NULL REFERENCES match_events(id) ON DELETE CASCADE,
    player_id UUID NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    max_played INTEGER NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, player_id)
);
