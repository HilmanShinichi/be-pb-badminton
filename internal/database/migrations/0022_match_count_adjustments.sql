-- Manual corrections on top of the counted matches, so a mis-recorded played or
-- refereed count can be fixed without touching the match cards. One row per
-- player and event; the deltas are summed into the counts everywhere.
CREATE TABLE IF NOT EXISTS match_count_adjustments (
    event_id UUID NOT NULL REFERENCES match_events(id) ON DELETE CASCADE,
    player_id UUID NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    played_delta INTEGER NOT NULL DEFAULT 0,
    refereed_delta INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, player_id)
);
