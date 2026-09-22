-- Court assignment per generated doubles match. 0 = unassigned.
ALTER TABLE generated_matches
    ADD COLUMN IF NOT EXISTS court INTEGER NOT NULL DEFAULT 0;
