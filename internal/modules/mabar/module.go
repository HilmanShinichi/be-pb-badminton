package mabar

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

func v64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func vDefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

type Session struct {
	ID               string  `json:"id"`
	Type             string  `json:"type"`
	PeriodID         *string `json:"period_id"`
	PeriodName       *string `json:"period_name"`
	VenueID          *string `json:"venue_id"`
	VenueName        *string `json:"venue_name"`
	Date             string  `json:"date"`
	StartTime        *string `json:"start_time"`
	EndTime          *string `json:"end_time"`
	DurationMinutes  *int    `json:"duration_minutes"`
	Description      *string `json:"description"`
	VenueDescription *string `json:"venue_description"`
	CourtCost        int64   `json:"court_cost"`
	PricingMode      *string `json:"pricing_mode"`
	ShuttlecockPrice int64   `json:"shuttlecock_price"`
	ShuttlePackPrice int64   `json:"shuttle_pack_price"`
	ShuttleUnitsPerPack int  `json:"shuttle_units_per_pack"`
	Status           string  `json:"status"`
	CreatedAt        string  `json:"created_at"`
}

type sessionInput struct {
	Type             *string `json:"type"`
	PeriodID         *string `json:"period_id"`
	VenueID          *string `json:"venue_id"`
	Date             *string `json:"date"`
	StartTime        *string `json:"start_time"`
	EndTime          *string `json:"end_time"`
	Description      *string `json:"description"`
	VenueDescription *string `json:"venue_description"`
	CourtCost        *int64  `json:"court_cost"`
	PricingMode      *string `json:"pricing_mode"`
	ShuttlecockPrice *int64  `json:"shuttlecock_price"`
	ShuttlePackPrice *int64  `json:"shuttle_pack_price"`
	ShuttleUnitsPerPack *int `json:"shuttle_units_per_pack"`
	Status           *string `json:"status"`
}

const sessionCols = `s.id::text, s.type, s.period_id::text, p.name, s.venue_id::text, v.name,
	s.date::text, s.start_time::text, s.end_time::text, s.duration_minutes, s.description,
	s.venue_description, s.court_cost, s.pricing_mode, s.shuttlecock_price,
	s.shuttle_pack_price, s.shuttle_units_per_pack, s.status, s.created_at::text`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.Type, &s.PeriodID, &s.PeriodName, &s.VenueID, &s.VenueName,
		&s.Date, &s.StartTime, &s.EndTime, &s.DurationMinutes, &s.Description,
		&s.VenueDescription, &s.CourtCost, &s.PricingMode, &s.ShuttlecockPrice,
		&s.ShuttlePackPrice, &s.ShuttleUnitsPerPack, &s.Status, &s.CreatedAt)
	return s, err
}

func (in sessionInput) validate() error {
	if in.Type == nil || !map[string]bool{"PERIOD": true, "DAILY_EVENT": true}[*in.Type] {
		return httpx.Unprocessable("Session type must be PERIOD or DAILY_EVENT.")
	}
	if in.Date == nil {
		return httpx.Unprocessable("Date is required.")
	}
	if _, err := time.Parse("2006-01-02", *in.Date); err != nil {
		return httpx.Unprocessable("Date must use YYYY-MM-DD format.")
	}
	if *in.Type == "PERIOD" && (in.PeriodID == nil || *in.PeriodID == "") {
		return httpx.Unprocessable("PERIOD sessions must belong to a period.")
	}
	if *in.Type == "DAILY_EVENT" && (in.VenueDescription == nil || strings.TrimSpace(*in.VenueDescription) == "") {
		return httpx.Unprocessable("Venue/court description is required for daily/event sessions.")
	}
	if in.CourtCost != nil && *in.CourtCost < 0 {
		return httpx.Unprocessable("Court cost must not be negative.")
	}
	if in.ShuttlecockPrice != nil && *in.ShuttlecockPrice < 0 {
		return httpx.Unprocessable("Shuttlecock price must not be negative.")
	}
	if in.ShuttlePackPrice != nil && *in.ShuttlePackPrice < 0 {
		return httpx.Unprocessable("Shuttle pack price must not be negative.")
	}
	if in.ShuttleUnitsPerPack != nil && *in.ShuttleUnitsPerPack <= 0 {
		return httpx.Unprocessable("Shuttle units per pack must be greater than 0.")
	}
	return nil
}

func (s *Service) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sql := `SELECT ` + sessionCols + ` FROM mabar_sessions s
		LEFT JOIN membership_periods p ON p.id = s.period_id
		LEFT JOIN venues v ON v.id = s.venue_id WHERE 1=1`
	args := []any{}
	add := func(clause string, val any) {
		args = append(args, val)
		sql += clause + strconv.Itoa(len(args))
	}
	if t := q.Get("type"); t != "" {
		add(` AND s.type = $`, t)
	}
	if pid := q.Get("period_id"); pid != "" {
		add(` AND s.period_id = $`, pid)
	}
	if from := q.Get("from"); from != "" {
		add(` AND s.date >= $`, from)
	}
	if to := q.Get("to"); to != "" {
		add(` AND s.date <= $`, to)
	}
	sql += ` ORDER BY s.date DESC LIMIT 100`

	rows, err := s.db.Query(r.Context(), sql, args...)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load sessions.")
		return
	}
	defer rows.Close()
	sessions := []Session{}
	for rows.Next() {
		if sess, err := scanSession(rows); err == nil {
			sessions = append(sessions, sess)
		}
	}
	httpx.OK(w, http.StatusOK, sessions)
}

func (s *Service) fetch(r *http.Request, id string) (Session, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Session{}, httpx.BadRequest("BAD_REQUEST", "Invalid session ID.")
	}
	sess, err := scanSession(s.db.QueryRow(r.Context(),
		`SELECT `+sessionCols+` FROM mabar_sessions s
		 LEFT JOIN membership_periods p ON p.id = s.period_id
		 LEFT JOIN venues v ON v.id = s.venue_id WHERE s.id = $1`, id))
	if err != nil {
		return Session{}, httpx.NotFound("Session not found.")
	}
	return sess, nil
}

func (s *Service) Get(w http.ResponseWriter, r *http.Request) {
	sess, err := s.fetch(r, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteAppError(w, err)
		return
	}
	httpx.OK(w, http.StatusOK, sess)
}

func (s *Service) Create(w http.ResponseWriter, r *http.Request) {
	var in sessionInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if err := in.validate(); err != nil {
		httpx.WriteAppError(w, err)
		return
	}
	if in.PeriodID != nil {
		var status string
		if err := s.db.QueryRow(r.Context(),
			`SELECT status FROM membership_periods WHERE id = $1`, *in.PeriodID).Scan(&status); err != nil {
			httpx.WriteAppError(w, httpx.NotFound("Period not found."))
			return
		}
		if status == "COMPLETED" {
			httpx.WriteAppError(w, httpx.Conflict("PERIOD_COMPLETED", "Period is completed and cannot be changed."))
			return
		}
	}
	var newID string
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save session.")
		return
	}
	defer tx.Rollback(r.Context())
	err = tx.QueryRow(r.Context(), `
		INSERT INTO mabar_sessions
		(id, type, period_id, venue_id, date, start_time, end_time, duration_minutes,
		 description, venue_description, court_cost, pricing_mode, shuttlecock_price,
		 shuttle_pack_price, shuttle_units_per_pack)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5::time, $6::time,
		 CASE WHEN $5::time IS NOT NULL AND $6::time IS NOT NULL
		      THEN (EXTRACT(EPOCH FROM ($6::time - $5::time)) / 60)::int ELSE NULL END,
		 $7, $8, $9, $10, $11, $12, $13)
		RETURNING id::text`,
		*in.Type, in.PeriodID, in.VenueID, *in.Date, in.StartTime, in.EndTime,
		in.Description, in.VenueDescription, v64(in.CourtCost), in.PricingMode, v64(in.ShuttlecockPrice),
		v64(in.ShuttlePackPrice), vDefInt(in.ShuttleUnitsPerPack, 12)).Scan(&newID)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save session.")
		return
	}
	// Period sessions start with every active member already PRESENT —
	// the admin then only marks whoever didn't show up.
	if in.PeriodID != nil {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO attendances (id, session_id, player_id, status, is_member)
			SELECT gen_random_uuid(), $1, m.player_id, 'PRESENT', true
			FROM memberships m
			JOIN players pl ON pl.id = m.player_id
			WHERE m.period_id = $2 AND m.status <> 'WITHDRAWN' AND pl.status <> 'ARCHIVED'`,
			newID, *in.PeriodID); err != nil {
			httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save session.")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save session.")
		return
	}
	sess, _ := s.fetch(r, newID)
	httpx.OK(w, http.StatusCreated, sess)
}

func (s *Service) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in sessionInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if err := in.validate(); err != nil {
		httpx.WriteAppError(w, err)
		return
	}
	tag, err := s.db.Exec(r.Context(), `
		UPDATE mabar_sessions SET
		 type = $2, period_id = $3, venue_id = $4, date = $5, start_time = $6::time, end_time = $7::time,
		 duration_minutes = CASE WHEN $6::time IS NOT NULL AND $7::time IS NOT NULL
		      THEN (EXTRACT(EPOCH FROM ($7::time - $6::time)) / 60)::int ELSE NULL END,
		 description = $8, venue_description = $9, court_cost = $10,
		 pricing_mode = $11, shuttlecock_price = $12,
		 shuttle_pack_price = $13, shuttle_units_per_pack = $14, updated_at = now()
		WHERE id = $1`,
		id, *in.Type, in.PeriodID, in.VenueID, *in.Date, in.StartTime, in.EndTime,
		in.Description, in.VenueDescription, v64(in.CourtCost), in.PricingMode, v64(in.ShuttlecockPrice),
		v64(in.ShuttlePackPrice), vDefInt(in.ShuttleUnitsPerPack, 12))
	if err != nil || tag.RowsAffected() == 0 {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Session not found.")
		return
	}
	sess, _ := s.fetch(r, id)
	httpx.OK(w, http.StatusOK, sess)
}

// Delete refuses when matches or financial records exist, unless
// ?force=true explicitly confirms wiping the whole session history
// (matches, attendance, simple recap, bills, payments, stock usage and
// session-scoped money records). PRD §50.
func (s *Service) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var matches, revs int
	_ = s.db.QueryRow(r.Context(), `SELECT COUNT(*) FROM matches WHERE session_id = $1`, id).Scan(&matches)
	_ = s.db.QueryRow(r.Context(), `SELECT COUNT(*) FROM revenues WHERE session_id = $1`, id).Scan(&revs)
	if (matches > 0 || revs > 0) && r.URL.Query().Get("force") != "true" {
		httpx.WriteAppError(w, httpx.Conflict("SESSION_HAS_HISTORY", "Session already has matches or transactions. Delete with force to wipe its history."))
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete session.")
		return
	}
	defer tx.Rollback(r.Context())
	for _, q := range []string{
		`DELETE FROM payments WHERE bill_id IN (SELECT id FROM player_bills WHERE session_id = $1)`,
		`DELETE FROM player_bills WHERE session_id = $1`,
		`DELETE FROM shuttlecock_transactions WHERE session_id = $1`,
		`DELETE FROM matches WHERE session_id = $1`,
		`DELETE FROM attendances WHERE session_id = $1`,
		`DELETE FROM session_player_stats WHERE session_id = $1`,
		`DELETE FROM revenues WHERE session_id = $1`,
		`DELETE FROM expenses WHERE session_id = $1`,
	} {
		if _, err := tx.Exec(r.Context(), q, id); err != nil {
			httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete session.")
			return
		}
	}
	tag, err := tx.Exec(r.Context(), `DELETE FROM mabar_sessions WHERE id = $1`, id)
	if err != nil || tag.RowsAffected() == 0 {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Session not found.")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete session.")
		return
	}
	httpx.OK(w, http.StatusOK, map[string]any{"ok": true})
}
