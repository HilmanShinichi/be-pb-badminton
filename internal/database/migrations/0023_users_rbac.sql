-- Role-based access: superadmins bypass every permission check, other
-- admins carry an explicit allowlist of feature keys (dashboard, mabar,
-- players, periods, matchmaker, inventory, finance, reports, simulator).
-- Existing accounts predate RBAC and keep full access as superadmins.
ALTER TABLE users ADD COLUMN IF NOT EXISTS is_superadmin BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS permissions TEXT[] NOT NULL DEFAULT '{}';
UPDATE users SET is_superadmin = TRUE;
