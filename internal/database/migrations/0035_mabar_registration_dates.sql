-- Add registration management fields to mabar_sessions
-- Allows admin to control when Open Play registration opens and closes

ALTER TABLE mabar_sessions
ADD COLUMN registration_opens_at TIMESTAMPTZ,
ADD COLUMN registration_closes_at TIMESTAMPTZ;

-- Add check constraint: registration must close after it opens
ALTER TABLE mabar_sessions
ADD CONSTRAINT check_registration_dates
CHECK (
  (registration_opens_at IS NULL AND registration_closes_at IS NULL)
  OR (registration_opens_at IS NOT NULL AND registration_closes_at IS NOT NULL)
  OR (registration_opens_at IS NOT NULL AND registration_closes_at > registration_opens_at)
);

-- Add index for queries filtering by registration status
CREATE INDEX idx_mabar_sessions_registration_dates
ON mabar_sessions (registration_opens_at, registration_closes_at)
WHERE registration_opens_at IS NOT NULL;
