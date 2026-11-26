-- Restore historical period attendances that were inadvertently converted to NOT_LISTED by migration 0034.
-- Only the newly created open play session (or sessions with active open play registration)
-- should keep unjoined members as NOT_LISTED.
UPDATE attendances a
SET status = 'PRESENT'
WHERE a.status = 'NOT_LISTED'
  AND a.is_member = true
  AND a.session_id IN (
    SELECT ms.id
    FROM mabar_sessions ms
    WHERE ms.id != '88b04e93-9a05-40e6-9a92-4bbccec67550'
      AND (ms.registration_opens_at IS NULL OR ms.date < '2026-11-28')
  );
