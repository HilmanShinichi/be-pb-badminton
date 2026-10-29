-- The round concept is gone: generated_matches is one flat chronology.
-- 1) Waves are renumbered to be globally increasing (they used to restart at 1
--    inside every round), keeping each former (round, wave) group together —
--    those are the matches that ran simultaneously.
-- 2) The round label itself is dropped.
UPDATE generated_matches m
SET wave = s.new_wave
FROM (
    SELECT id, dense_rank() OVER (ORDER BY round, wave) AS new_wave
    FROM generated_matches
) s
WHERE m.id = s.id;

ALTER TABLE generated_matches DROP COLUMN round;
