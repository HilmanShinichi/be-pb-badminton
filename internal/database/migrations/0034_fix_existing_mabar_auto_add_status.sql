-- Migration 0034: Fix existing mabar auto-added members status
-- Purpose: Update auto-added members from PRESENT to NOT_LISTED
--          for existing period-type mabar sessions created before the fix

-- Update attendances for period sessions where members were auto-added
-- (identified by status = 'PRESENT', is_member = true)
UPDATE attendances a
SET status = 'NOT_LISTED'
WHERE a.session_id IN (
  SELECT ms.id
  FROM mabar_sessions ms
  WHERE ms.type = 'PERIOD'
    AND ms.period_id IS NOT NULL
)
AND a.status = 'PRESENT'
AND a.is_member = true;
