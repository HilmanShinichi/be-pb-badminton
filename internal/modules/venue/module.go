package venue

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

func (s *Service) ListVenues(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(),
		`SELECT v.id::text, v.name, v.address, v.notes,
		        COALESCE((SELECT COUNT(*) FROM courts c WHERE c.venue_id = v.id), 0)
		 FROM venues v ORDER BY v.name`)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load venues.")
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id, name string
		var address, notes *string
		var courts int
		if err := rows.Scan(&id, &name, &address, &notes, &courts); err == nil {
			list = append(list, map[string]any{"id": id, "name": name, "address": address, "notes": notes, "court_count": courts})
		}
	}
	httpx.OK(w, http.StatusOK, list)
}

func (s *Service) CreateVenue(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Address, Notes *string }
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		httpx.WriteAppError(w, httpx.Unprocessable("Nama venue is required."))
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(),
		`INSERT INTO venues (id, name, address, notes) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING id::text`,
		strings.TrimSpace(*in.Name), in.Address, in.Notes).Scan(&id)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save venue.")
		return
	}
	httpx.OK(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Service) ListCourts(w http.ResponseWriter, r *http.Request) {
	sql := `SELECT c.id::text, c.venue_id::text, v.name, c.name, c.notes
		FROM courts c JOIN venues v ON v.id = c.venue_id`
	args := []any{}
	if vid := r.URL.Query().Get("venue_id"); vid != "" {
		args = append(args, vid)
		sql += ` WHERE c.venue_id = $1`
	}
	sql += ` ORDER BY v.name, c.name`
	rows, err := s.db.Query(r.Context(), sql, args...)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load courts.")
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id, venueID, venueName, name string
		var notes *string
		if err := rows.Scan(&id, &venueID, &venueName, &name, &notes); err == nil {
			list = append(list, map[string]any{"id": id, "venue_id": venueID, "venue_name": venueName, "name": name, "notes": notes})
		}
	}
	httpx.OK(w, http.StatusOK, list)
}

func (s *Service) CreateCourt(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VenueID string  `json:"venue_id"`
		Name    *string `json:"name"`
		Notes   *string `json:"notes"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		httpx.WriteAppError(w, httpx.Unprocessable("Nama court is required."))
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(),
		`INSERT INTO courts (id, venue_id, name, notes) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING id::text`,
		in.VenueID, strings.TrimSpace(*in.Name), in.Notes).Scan(&id)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save court. Make sure the venue exists.")
		return
	}
	httpx.OK(w, http.StatusCreated, map[string]any{"id": id})
}
