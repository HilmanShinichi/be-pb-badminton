package mabar

import (
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/pb-kecebong/backend/internal/httpx"
)

var attendanceStatuses = map[string]bool{
	"NOT_LISTED": true, "LISTED": true, "CONFIRMED": true, "PRESENT": true,
	"CANCELLED": true, "ABSENT": true, "NO_SHOW": true,
}

type Attendance struct {
	ID                     string  `json:"id"`
	SessionID              string  `json:"session_id"`
	PlayerID               string  `json:"player_id"`
	PlayerName             string  `json:"player_name"`
	Status                 string  `json:"status"`
	IsMember               bool    `json:"is_member"`
	ReplacementForPlayerID *string `json:"replacement_for_player_id"`
	ListedAt               string  `json:"listed_at"`
	CancelledAt            *string `json:"cancelled_at"`
	NoShowReason           *string `json:"no_show_reason"`
}

func (s *Service) ListAttendance(c *fiber.Ctx) error {
	id := c.Params("id")
	rows, err := s.db.Query(c.Context(), `
		SELECT a.id::text, a.session_id::text, a.player_id::text, pl.name, a.status,
		       a.is_member, a.replacement_for_player_id::text, a.listed_at::text,
		       a.cancelled_at::text, a.no_show_reason
		FROM attendances a
		JOIN players pl ON pl.id = a.player_id
		WHERE a.session_id = $1 ORDER BY pl.name`, id)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load attendance.")
	}
	defer rows.Close()
	list := []Attendance{}
	for rows.Next() {
		var a Attendance
		if err := rows.Scan(&a.ID, &a.SessionID, &a.PlayerID, &a.PlayerName, &a.Status,
			&a.IsMember, &a.ReplacementForPlayerID, &a.ListedAt, &a.CancelledAt, &a.NoShowReason); err == nil {
			list = append(list, a)
		}
	}
	return httpx.OK(c, http.StatusOK, list)
}

type attendanceInput struct {
	PlayerID               string  `json:"player_id"`
	Status                 string  `json:"status"`
	IsMember               bool    `json:"is_member"`
	ReplacementForPlayerID *string `json:"replacement_for_player_id"`
	NoShowReason           *string `json:"no_show_reason"`
}

// SeedMembers inserts every active period member as PRESENT without
// touching rows that already exist (late joiners appear, manual ABSENT
// marks stay), so the admin marks exceptions instead of every player.
type bulkAttendanceInput struct {
	Players     []attendanceInput `json:"players"`
	PresentAll  bool              `json:"present_all"`
	SeedMembers bool              `json:"seed_members"`
	Overrides   []attendanceInput `json:"overrides"`
}

// SetAttendance upserts the full attendance sheet of one session in one call.
func (s *Service) SetAttendance(c *fiber.Ctx) error {
	id := c.Params("id")
	sess, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	if sess.PeriodID != nil {
		var status string
		_ = s.db.QueryRow(c.Context(),
			`SELECT status FROM membership_periods WHERE id = $1`, *sess.PeriodID).Scan(&status)
		if status == "COMPLETED" {
			return httpx.WriteAppError(c, httpx.Conflict("PERIOD_COMPLETED", "Period is completed and cannot be changed."))
		}
	}

	var in bulkAttendanceInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}

	tx, err := s.db.Begin(c.Context())
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save attendance.")
	}
	defer tx.Rollback(c.Context())

	if in.PresentAll {
		if _, err := tx.Exec(c.Context(), `
			UPDATE attendances SET status = 'PRESENT'
			WHERE session_id = $1 AND status IN ('LISTED', 'CONFIRMED')`, id); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save attendance.")
		}
	}

	if in.SeedMembers && sess.PeriodID != nil {
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO attendances (id, session_id, player_id, status, is_member)
			SELECT gen_random_uuid(), $1, m.player_id, 'PRESENT', true
			FROM memberships m
			JOIN players pl ON pl.id = m.player_id
			WHERE m.period_id = $2 AND m.status <> 'WITHDRAWN' AND pl.status <> 'ARCHIVED'
			ON CONFLICT (session_id, player_id) DO NOTHING`, id, *sess.PeriodID); err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save attendance.")
		}
	}

	apply := func(it attendanceInput) error {
		if _, err := uuid.Parse(it.PlayerID); err != nil {
			return httpx.BadRequest("BAD_REQUEST", "Invalid player ID.")
		}
		if !attendanceStatuses[it.Status] {
			return httpx.Unprocessable("Invalid attendance status: " + it.Status)
		}
		_, err := tx.Exec(c.Context(), `
			INSERT INTO attendances (id, session_id, player_id, status, is_member,
			                         replacement_for_player_id, cancelled_at, no_show_reason)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5,
			        CASE WHEN $3 = 'CANCELLED' THEN now() END, $6)
			ON CONFLICT (session_id, player_id) DO UPDATE SET
				status = EXCLUDED.status,
				is_member = EXCLUDED.is_member,
				replacement_for_player_id = EXCLUDED.replacement_for_player_id,
				cancelled_at = CASE WHEN EXCLUDED.status = 'CANCELLED' THEN now() ELSE NULL END,
				no_show_reason = EXCLUDED.no_show_reason`,
			id, it.PlayerID, it.Status, it.IsMember, it.ReplacementForPlayerID, it.NoShowReason)
		return err
	}

	for _, it := range in.Players {
		if err := apply(it); err != nil {
			return httpx.WriteAppError(c, err)
		}
	}
	for _, it := range in.Overrides {
		if err := apply(it); err != nil {
			return httpx.WriteAppError(c, err)
		}
	}
	if err := tx.Commit(c.Context()); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save attendance.")
	}
	return s.ListAttendance(c)
}

// RemoveAttendance deletes one player's attendance row from a session. It is
// refused when the player already has bills in the session (money trail must
// stay: set status to CANCELLED instead). When the player has no other
// history anywhere (no attendance, matches, bills, payments, revenues,
// memberships, or recap stats) the player row itself is hard-deleted too, so
// a typo name added via quick-add disappears completely.
func (s *Service) RemoveAttendance(c *fiber.Ctx) error {
	id := c.Params("id")
	playerID := c.Params("playerId")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid session ID."))
	}
	if _, err := uuid.Parse(playerID); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid player ID."))
	}
	sess, err := s.fetch(c.Context(), id)
	if err != nil {
		return httpx.WriteAppError(c, err)
	}
	if sess.PeriodID != nil {
		var status string
		_ = s.db.QueryRow(c.Context(),
			`SELECT status FROM membership_periods WHERE id = $1`, *sess.PeriodID).Scan(&status)
		if status == "COMPLETED" {
			return httpx.WriteAppError(c, httpx.Conflict("PERIOD_COMPLETED", "Period is completed and cannot be changed."))
		}
	}

	var bills int
	_ = s.db.QueryRow(c.Context(),
		`SELECT COUNT(*) FROM player_bills WHERE session_id = $1 AND player_id = $2`,
		id, playerID).Scan(&bills)
	if bills > 0 {
		return httpx.WriteAppError(c, httpx.Conflict("ATTENDANCE_HAS_BILLS",
			"Player already has bills in this session. Set status to CANCELLED instead of removing."))
	}

	tag, err := s.db.Exec(c.Context(),
		`DELETE FROM attendances WHERE session_id = $1 AND player_id = $2`, id, playerID)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Attendance not found.")
	}

	deleted := false
	var history int
	_ = s.db.QueryRow(c.Context(), `
		SELECT (SELECT COUNT(*) FROM attendances WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM match_players WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM player_bills WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM payments WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM revenues WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM memberships WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM session_player_stats WHERE player_id = $1)`,
		playerID).Scan(&history)
	if history == 0 {
		tag, err := s.db.Exec(c.Context(), `DELETE FROM players WHERE id = $1`, playerID)
		if err == nil && tag.RowsAffected() > 0 {
			deleted = true
		}
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true, "player_deleted": deleted})
}

// AttendanceStats answers the no-show questions per session. PRD §17.
func (s *Service) AttendanceStats(c *fiber.Ctx) error {
	id := c.Params("id")
	var listed, present, cancelled, noShow int
	err := s.db.QueryRow(c.Context(), `
		SELECT
			COUNT(*) FILTER (WHERE status <> 'NOT_LISTED'),
			COUNT(*) FILTER (WHERE status = 'PRESENT'),
			COUNT(*) FILTER (WHERE status = 'CANCELLED'),
			COUNT(*) FILTER (WHERE status = 'NO_SHOW')
		FROM attendances WHERE session_id = $1`, id).Scan(&listed, &present, &cancelled, &noShow)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not compute statistics.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{
		"listed":          listed,
		"present":         present,
		"cancelled":       cancelled,
		"no_show":         noShow,
		"no_show_rate_bp": noShowRate(noShow, listed),
	})
}

func noShowRate(noShow, listed int) int64 {
	if listed == 0 {
		return 0
	}
	return int64(noShow * 1000 / listed)
}
