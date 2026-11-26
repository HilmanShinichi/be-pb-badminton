-- Add admin note field to open_play_sessions
ALTER TABLE open_play_sessions ADD COLUMN admin_note TEXT;

-- Index for consistency
CREATE INDEX idx_open_play_sessions_admin_note ON open_play_sessions (id);
