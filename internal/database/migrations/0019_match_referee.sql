-- One referee per generated doubles match. Clearing the player keeps history.
ALTER TABLE generated_matches
    ADD COLUMN IF NOT EXISTS referee_id UUID REFERENCES players(id) ON DELETE SET NULL;
