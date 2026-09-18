package player

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

type Player struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Phone     *string `json:"phone"`
	Notes     *string `json:"notes"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"created_at"`
}

type playerInput struct {
	Name   *string `json:"name"`
	Phone  *string `json:"phone"`
	Notes  *string `json:"notes"`
	Status *string `json:"status"`
}

func (s *Service) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	search := strings.TrimSpace(q.Get("q"))
	status := q.Get("status")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))

	sql := `SELECT id::text, name, phone, notes, status, created_at::text
		FROM players WHERE status <> 'ARCHIVED'`
	args := []any{}
	if search != "" {
		args = append(args, "%"+search+"%")
		sql += ` AND (name ILIKE $` + strconv.Itoa(len(args)) + ` OR phone ILIKE $` + strconv.Itoa(len(args)) + `)`
	}
	if status != "" {
		args = append(args, status)
		sql += ` AND status = $` + strconv.Itoa(len(args))
	}
	sql += ` ORDER BY name LIMIT ` + strconv.Itoa(limit) + ` OFFSET ` + strconv.Itoa(offset)

	rows, err := s.db.Query(r.Context(), sql, args...)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load players.")
		return
	}
	defer rows.Close()

	players := []Player{}
	for rows.Next() {
		var p Player
		if err := rows.Scan(&p.ID, &p.Name, &p.Phone, &p.Notes, &p.Status, &p.CreatedAt); err == nil {
			players = append(players, p)
		}
	}
	httpx.OKWithMeta(w, http.StatusOK, players, map[string]any{"limit": limit, "offset": offset})
}

func (s *Service) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid player ID.")
		return
	}
	var p Player
	err := s.db.QueryRow(r.Context(),
		`SELECT id::text, name, phone, notes, status, created_at::text FROM players WHERE id = $1`, id,
	).Scan(&p.ID, &p.Name, &p.Phone, &p.Notes, &p.Status, &p.CreatedAt)
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Player not found.")
		return
	}
	httpx.OK(w, http.StatusOK, p)
}

func (s *Service) validate(in playerInput) error {
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return httpx.Unprocessable("Player name is required.")
	}
	if in.Status != nil && !map[string]bool{"ACTIVE": true, "INACTIVE": true, "ARCHIVED": true}[*in.Status] {
		return httpx.Unprocessable("Invalid player status.")
	}
	return nil
}

func (s *Service) Create(w http.ResponseWriter, r *http.Request) {
	var in playerInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if err := s.validate(in); err != nil {
		httpx.WriteAppError(w, err)
		return
	}
	var p Player
	err := s.db.QueryRow(r.Context(),
		`INSERT INTO players (id, name, phone, notes) VALUES (gen_random_uuid(), $1, $2, $3)
		 RETURNING id::text, name, phone, notes, status, created_at::text`,
		strings.TrimSpace(*in.Name), in.Phone, in.Notes,
	).Scan(&p.ID, &p.Name, &p.Phone, &p.Notes, &p.Status, &p.CreatedAt)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save player.")
		return
	}
	httpx.OK(w, http.StatusCreated, p)
}

func (s *Service) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid player ID.")
		return
	}
	var in playerInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if err := s.validate(in); err != nil {
		httpx.WriteAppError(w, err)
		return
	}
	var p Player
	err := s.db.QueryRow(r.Context(),
		`UPDATE players SET name = $2, phone = $3, notes = $4,
		 status = COALESCE($5, status), updated_at = now()
		 WHERE id = $1
		 RETURNING id::text, name, phone, notes, status, created_at::text`,
		id, strings.TrimSpace(*in.Name), in.Phone, in.Notes, in.Status,
	).Scan(&p.ID, &p.Name, &p.Phone, &p.Notes, &p.Status, &p.CreatedAt)
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Player not found.")
		return
	}
	httpx.OK(w, http.StatusOK, p)
}

// Delete hard-deletes a player, but only when nothing references them:
// attendance, matches, bills, payments, revenues, memberships, or recap
// stats. A player with any history is refused so money trails and past
// sessions stay intact.
func (s *Service) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid player ID.")
		return
	}
	var refs int
	_ = s.db.QueryRow(r.Context(), `
		SELECT (SELECT COUNT(*) FROM attendances WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM match_players WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM player_bills WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM payments WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM revenues WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM memberships WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM session_player_stats WHERE player_id = $1)`,
		id).Scan(&refs)
	if refs > 0 {
		httpx.WriteAppError(w, httpx.Conflict("PLAYER_HAS_HISTORY",
			"Player already has attendance, bills, or other history and cannot be deleted."))
		return
	}
	tag, err := s.db.Exec(r.Context(), `DELETE FROM players WHERE id = $1`, id)
	if err != nil || tag.RowsAffected() == 0 {
		httpx.Err(w, http.StatusNotFound, "NOT_FOUND", "Player not found.")
		return
	}
	httpx.OK(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}
