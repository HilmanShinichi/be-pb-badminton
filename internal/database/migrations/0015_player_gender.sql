-- Player gender: L = male, P = female. NULL = unknown (legacy rows).
ALTER TABLE players
    ADD COLUMN IF NOT EXISTS gender TEXT CHECK (gender IS NULL OR gender IN ('L', 'P'));
