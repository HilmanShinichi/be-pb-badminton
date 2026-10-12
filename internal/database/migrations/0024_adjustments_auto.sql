-- Distinguish automatic newcomer base offsets from deliberate manual
-- corrections: only auto rows follow later base changes, and auto rows
-- never stack on repeat adds.
ALTER TABLE match_count_adjustments ADD COLUMN IF NOT EXISTS is_auto BOOLEAN NOT NULL DEFAULT FALSE;
