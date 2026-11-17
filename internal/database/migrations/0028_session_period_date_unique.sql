-- One session per period per date. The create form could previously be
-- submitted twice for the same period + date, so clean the existing
-- duplicates first: keep the session that already carries data (paid bills
-- first, then non-member attendance) and fold the empty copy into it.
-- Then a partial unique index locks the rule in for every future write.

DROP TABLE IF EXISTS pg_temp.dup_sessions;

CREATE TEMP TABLE dup_sessions ON COMMIT DROP AS
WITH scored AS (
    SELECT s.id, s.period_id, s.date, s.created_at,
           (SELECT count(*) FROM player_bills b
             WHERE b.session_id = s.id AND b.payment_status = 'PAID') AS paid_bills,
           (SELECT count(*) FROM player_bills b WHERE b.session_id = s.id) AS bills,
           (SELECT count(*) FROM attendances a
             WHERE a.session_id = s.id AND a.is_member = FALSE) AS guests,
           (SELECT count(*) FROM attendances a WHERE a.session_id = s.id) AS attendance,
           (SELECT count(*) FROM matches m WHERE m.session_id = s.id) AS matches,
           (SELECT count(*) FROM revenues r WHERE r.session_id = s.id)
         + (SELECT count(*) FROM expenses e WHERE e.session_id = s.id)
         + (SELECT count(*) FROM shuttlecock_transactions t WHERE t.session_id = s.id) AS money
    FROM mabar_sessions s
    WHERE s.period_id IS NOT NULL
      AND EXISTS (SELECT 1 FROM mabar_sessions x
                   WHERE x.period_id = s.period_id AND x.date = s.date AND x.id <> s.id)
),
ranked AS (
    SELECT id, keeper_id, rn
    FROM (
        SELECT id,
               first_value(id) OVER w AS keeper_id,
               row_number() OVER w AS rn
        FROM scored
        WINDOW w AS (PARTITION BY period_id, date
                     ORDER BY paid_bills DESC, bills DESC, guests DESC, attendance DESC,
                              matches DESC, money DESC, created_at ASC)
    ) x
    WHERE rn > 1
)
SELECT * FROM ranked;

-- Losers are emptied into their keeper one group at a time, so unique keys
-- (session_id, player_id) / (session_id, sequence) never collide: rows the
-- keeper already owns are dropped, the rest are moved over.
DO $$
DECLARE
    d       RECORD;
    base    INT;
BEGIN
    FOR d IN SELECT id, keeper_id FROM dup_sessions ORDER BY rn LOOP
        DELETE FROM attendances a
         WHERE a.session_id = d.id
           AND EXISTS (SELECT 1 FROM attendances k
                        WHERE k.session_id = d.keeper_id AND k.player_id = a.player_id);
        UPDATE attendances SET session_id = d.keeper_id WHERE session_id = d.id;

        DELETE FROM session_player_stats s
         WHERE s.session_id = d.id
           AND EXISTS (SELECT 1 FROM session_player_stats k
                        WHERE k.session_id = d.keeper_id AND k.player_id = s.player_id);
        UPDATE session_player_stats SET session_id = d.keeper_id WHERE session_id = d.id;

        -- Bill collision for the same player: merge the loser's payments
        -- into the keeper's bill, keep whichever side was paid, drop the copy.
        UPDATE payments p
           SET bill_id = k.id
          FROM player_bills lb, player_bills k
         WHERE p.bill_id = lb.id AND lb.session_id = d.id
           AND k.session_id = d.keeper_id AND k.player_id = lb.player_id;

        UPDATE player_bills k
           SET payment_status = 'PAID'
          FROM player_bills lb
         WHERE lb.session_id = d.id AND k.session_id = d.keeper_id
           AND k.player_id = lb.player_id
           AND lb.payment_status = 'PAID' AND k.payment_status <> 'PAID';

        DELETE FROM player_bills lb
         WHERE lb.session_id = d.id
           AND EXISTS (SELECT 1 FROM player_bills k
                        WHERE k.session_id = d.keeper_id AND k.player_id = lb.player_id);
        UPDATE player_bills SET session_id = d.keeper_id WHERE session_id = d.id;

        -- Matches are renumbered after the keeper's last sequence.
        SELECT COALESCE(max(sequence), 0) INTO base
          FROM matches WHERE session_id = d.keeper_id;
        UPDATE matches m
           SET session_id = d.keeper_id,
               sequence = base + rn.n
          FROM (SELECT id, row_number() OVER (ORDER BY sequence, id) AS n
                  FROM matches WHERE session_id = d.id) rn
         WHERE m.id = rn.id;

        UPDATE revenues SET session_id = d.keeper_id WHERE session_id = d.id;
        UPDATE expenses SET session_id = d.keeper_id WHERE session_id = d.id;
        UPDATE shuttlecock_transactions SET session_id = d.keeper_id WHERE session_id = d.id;
        UPDATE match_events SET source_session_id = d.keeper_id WHERE source_session_id = d.id;

        DELETE FROM mabar_sessions WHERE id = d.id;
    END LOOP;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_mabar_sessions_period_date
    ON mabar_sessions (period_id, date)
    WHERE period_id IS NOT NULL;
