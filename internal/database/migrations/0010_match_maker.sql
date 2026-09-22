-- Player skill grades (e.g. A1 superior to A2; mains A, B, C) plus the
-- doubles match-maker: events own a player pool, generated 2v2 matchups are
-- stored as team objects so names/grades can be edited after generation.
ALTER TABLE players
    ADD COLUMN IF NOT EXISTS grade TEXT;

CREATE TABLE IF NOT EXISTS match_events (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'OPEN',
    player_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS generated_matches (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   UUID NOT NULL REFERENCES match_events(id) ON DELETE CASCADE,
    round      INTEGER NOT NULL DEFAULT 1,
    team1      JSONB NOT NULL DEFAULT '[]'::jsonb,
    team2      JSONB NOT NULL DEFAULT '[]'::jsonb,
    status     TEXT NOT NULL DEFAULT 'UPCOMING',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_generated_matches_event ON generated_matches (event_id);
