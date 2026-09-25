-- Wave (time slot) per generated match: matches in the same wave run
-- simultaneously, so a referee must come from a later wave or sit out.
ALTER TABLE generated_matches
    ADD COLUMN IF NOT EXISTS wave INTEGER NOT NULL DEFAULT 1;
