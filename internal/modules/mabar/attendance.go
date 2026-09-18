package mabar

import (
	"net/http"

	"github.com/go-chi/chi/v5"
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

func (s *Service) ListAttendance(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rows, err := s.db.Query(r.Context(), `
		SELECT a.id::text, a.session_id::text, a.player_id::text, pl.name, a.status,
		       a.is_member, a.replacement_for_player_id::text, a.listed_at::text,
		       a.cancelled_at::text, a.no_show_reason
		FROM attendances a
		JOIN players pl ON pl.id = a.player_id
		WHERE a.session_id = $1 ORDER BY pl.name`, id)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load attendance.")
		return
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
	httpx.OK(w, http.StatusOK, list)
}

type attendanceInput struct {
	PlayerID               string  `json:"player_id"`
	Status                 string  `json:"status"`
	IsMember               bool    `json:"is_member"`
	ReplacementForPlayerID *string `json:"replacement_for_player_id"`
	NoShowReason           *string `json:"no_show_reason"`
}

type bulkAttendanceInput struct {
	Players []attendanceInput `json:"players"`
	// PresentAll flips every current LISTED/CONFIRMED entry to PRESENT first,
	// so the admin marks exceptions instead of every player. PRD §54.
	PresentAll bool              `json:"present_all"`
	Overrides  []attendanceInput `json:"overrides"`
}

// SetAttendance upserts the full attendance sheet of one session in one call.
func (s *Service) SetAttendance(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.fetch(r, id)
	if err != nil {
		httpx.WriteAppError(w, err)
		return
	}
	if sess.PeriodID != nil {
		var status string
		_ = s.db.QueryRow(r.Context(),
			`SELECT status FROM membership_periods WHERE id = $1`, *sess.PeriodID).Scan(&status)
		if status == "COMPLETED" {
			httpx.WriteAppError(w, httpx.Conflict("PERIOD_COMPLETED", "Period is completed and cannot be changed."))
			return
		}
	}

	var in bulkAttendanceInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save attendance.")
		return
	}
	defer tx.Rollback(r.Context())

	if in.PresentAll {
		if _, err := tx.Exec(r.Context(), `
			UPDATE attendances SET status = 'PRESENT'
			WHERE session_id = $1 AND status IN ('LISTED', 'CONFIRMED')`, id); err != nil {
			httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save attendance.")
			return
		}
	}

	apply := func(it attendanceInput) error {
		if _, err := uuid.Parse(it.PlayerID); err != nil {
			return httpx.BadRequest("BAD_REQUEST", "Invalid player ID.")
		}
		if !attendanceStatuses[it.Status] {
			return httpx.Unprocessable("Invalid attendance status: " + it.Status)
		}
		_, err := tx.Exec(r.Context(), `
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
			httpx.WriteAppError(w, err)
			return
		}
	}
	for _, it := range in.Overrides {
		if err := apply(it); err != nil {
			httpx.WriteAppError(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save attendance.")
		return
	}
	s.ListAttendance(w, r)
}

// RemoveAttendance deletes one player's attendance row from a session. It is
// refused when the player already has bills in the session (money trail must
// stay: set status to CANCELLED instead). When the player has no other
// history anywhere (no attendance, matches, bills, payments, revenues,
// memberships, or recap stats) the player row itself is hard-deleted too, so
// a typo name added via quick-add disappears completely.
func (s *Service) RemoveAttendance(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playerID := chi.URLParam(r, "playerId")
	if _, err := uuid.Parse(id); err != nil {
		httpx.WriteAppError(w, httpx.BadRequest("BAD_REQUEST", "Invalid session ID."))
		return
	}
	if _, err := uuid.Parse(playerID); err != nil {
		httpx.WriteAppError(w, httpx.BadRequest("BAD_REQUEST", "Invalid player ID."))
		return
	}
	sess, err := s.fetch(r, id)
	if err != nil {
		httpx.WriteAppError(w, err)
		return
	}
	if sess.PeriodID != nil {
		var status string
		_ = s.db.QueryRow(r.Context(),
			`SELECT status FROM membership_periods WHERE id = $1`, *sess.PeriodID).Scan(&status)
		if status == "COMPLETED" {
			httpx.WriteAppError(w, httpx.Conflict("PERIOD_COMPLETED", "Period is completed and cannot be changed."))
			return
		}
	}

	var bills int
	_ = s.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM player_bills WHERE session_id = $1 AND player_id = $2`,
		id, playerID).Scan(&bills)
	if bills > 0 {
		httpx.WriteAppError(w, httpx.Conflict("ATTENDANCE_HAS_BILLS",
			"Player already has bills in this session. Set status to CANCELLED instead of removing."))
		return
	}

	tag, err := s.db.Exec(r.Context(),
		`DELETE FROM attendances WHERE session_id = $1 AND player_id = $2`, id, playerID)
	if err != nil || tag.RowsAffected() == 0 {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Attendance not found.")
		return
	}

	deleted := false
	var history int
	_ = s.db.QueryRow(r.Context(), `
		SELECT (SELECT COUNT(*) FROM attendances WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM match_players WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM player_bills WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM payments WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM revenues WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM memberships WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM session_player_stats WHERE player_id = $1)`,
		playerID).Scan(&history)
	if history == 0 {
		tag, err := s.db.Exec(r.Context(), `DELETE FROM players WHERE id = $1`, playerID)
		if err == nil && tag.RowsAffected() > 0 {
			deleted = true
		}
	}
	httpx.OK(w, http.StatusOK, map[string]any{"ok": true, "player_deleted": deleted})
}

// AttendanceStats answers the no-show questions per session. PRD §17.
func (s *Service) AttendanceStats(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var listed, present, cancelled, noShow int
	err := s.db.QueryRow(r.Context(), `
		SELECT
			COUNT(*) FILTER (WHERE status <> 'NOT_LISTED'),
			COUNT(*) FILTER (WHERE status = 'PRESENT'),
			COUNT(*) FILTER (WHERE status = 'CANCELLED'),
			COUNT(*) FILTER (WHERE status = 'NO_SHOW')
		FROM attendances WHERE session_id = $1`, id).Scan(&listed, &present, &cancelled, &noShow)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not compute statistics.")
		return
	}
	httpx.OK(w, http.StatusOK, map[string]any{
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
