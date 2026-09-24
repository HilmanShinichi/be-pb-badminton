package player

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
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
	Grade     *string `json:"grade"`
	Gender    *string `json:"gender"`
	CreatedAt string  `json:"created_at"`
}

type playerInput struct {
	Name   *string `json:"name"`
	Phone  *string `json:"phone"`
	Notes  *string `json:"notes"`
	Status *string `json:"status"`
	Grade  *string `json:"grade"`
	Gender *string `json:"gender"`
}

// validGrades: mains A, B, C with sub-grades 1-3. Lower number is superior
// within a main (A1 outranks A2).
var validGrades = map[string]bool{
	"A1": true, "A2": true, "A3": true,
	"B1": true, "B2": true, "B3": true,
	"C1": true, "C2": true, "C3": true,
}

// GradeRank orders grades for balanced-team generation: A1 strongest.
func GradeRank(g string) int {
	switch g {
	case "A1":
		return 1
	case "A2":
		return 2
	case "A3":
		return 3
	case "B1":
		return 4
	case "B2":
		return 5
	case "B3":
		return 6
	case "C1":
		return 7
	case "C2":
		return 8
	case "C3":
		return 9
	}
	return 99
}

func normalizeGrade(in *string) (*string, error) {
	if in == nil {
		return nil, nil
	}
	g := strings.ToUpper(strings.TrimSpace(*in))
	if g == "" {
		return nil, nil
	}
	if !validGrades[g] {
		return nil, httpx.Unprocessable("Invalid grade. Use A1-A3, B1-B3, or C1-C3.")
	}
	return &g, nil
}

// normalizeGender accepts only L (male) or P (female); empty clears to NULL.
func normalizeGender(in *string) (*string, error) {
	if in == nil {
		return nil, nil
	}
	g := strings.ToUpper(strings.TrimSpace(*in))
	if g == "" {
		return nil, nil
	}
	if g != "L" && g != "P" {
		return nil, httpx.Unprocessable("Invalid gender. Use L or P.")
	}
	return &g, nil
}

func (s *Service) List(c *fiber.Ctx) error {
	search := strings.TrimSpace(c.Query("q"))
	status := c.Query("status")
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(c.Query("offset"))

	sql := `SELECT id::text, name, phone, notes, status, grade, gender, created_at::text
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
	if g := strings.ToUpper(strings.TrimSpace(c.Query("grade"))); g != "" {
		if g == "NONE" {
			sql += ` AND grade IS NULL`
		} else {
			args = append(args, g)
			sql += ` AND grade = $` + strconv.Itoa(len(args))
		}
	}
	if g := strings.ToUpper(strings.TrimSpace(c.Query("gender"))); g != "" {
		args = append(args, g)
		sql += ` AND gender = $` + strconv.Itoa(len(args))
	}
	sql += ` ORDER BY name LIMIT ` + strconv.Itoa(limit) + ` OFFSET ` + strconv.Itoa(offset)

	rows, err := s.db.Query(c.Context(), sql, args...)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load players.")
	}
	defer rows.Close()

	players := []Player{}
	for rows.Next() {
		var p Player
		if err := rows.Scan(&p.ID, &p.Name, &p.Phone, &p.Notes, &p.Status, &p.Grade, &p.Gender, &p.CreatedAt); err == nil {
			players = append(players, p)
		}
	}
	return httpx.OKWithMeta(c, http.StatusOK, players, map[string]any{"limit": limit, "offset": offset})
}

func (s *Service) Get(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid player ID.")
	}
	var p Player
	err := s.db.QueryRow(c.Context(),
		`SELECT id::text, name, phone, notes, status, grade, gender, created_at::text FROM players WHERE id = $1`, id,
	).Scan(&p.ID, &p.Name, &p.Phone, &p.Notes, &p.Status, &p.Grade, &p.Gender, &p.CreatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Player not found.")
	}
	return httpx.OK(c, http.StatusOK, p)
}

func (s *Service) validate(in playerInput) error {
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return httpx.Unprocessable("Player name is required.")
	}
	if in.Status != nil && !map[string]bool{"ACTIVE": true, "INACTIVE": true, "ARCHIVED": true}[*in.Status] {
		return httpx.Unprocessable("Invalid player status.")
	}
	if in.Grade != nil {
		if _, err := normalizeGrade(in.Grade); err != nil {
			return err
		}
	}
	if in.Gender != nil {
		if _, err := normalizeGender(in.Gender); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Create(c *fiber.Ctx) error {
	var in playerInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if err := s.validate(in); err != nil {
		return httpx.WriteAppError(c, err)
	}
	grade, _ := normalizeGrade(in.Grade)
	gender, _ := normalizeGender(in.Gender)
	var p Player
	err := s.db.QueryRow(c.Context(),
		`INSERT INTO players (id, name, phone, notes, grade, gender) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)
		 RETURNING id::text, name, phone, notes, status, grade, gender, created_at::text`,
		strings.TrimSpace(*in.Name), in.Phone, in.Notes, grade, gender,
	).Scan(&p.ID, &p.Name, &p.Phone, &p.Notes, &p.Status, &p.Grade, &p.Gender, &p.CreatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not save player.")
	}
	return httpx.OK(c, http.StatusCreated, p)
}

func (s *Service) Update(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid player ID.")
	}
	var in playerInput
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if err := s.validate(in); err != nil {
		return httpx.WriteAppError(c, err)
	}
	grade, _ := normalizeGrade(in.Grade)
	gender, _ := normalizeGender(in.Gender)
	// Re-assigning the grade the player already holds is rejected so
	// duplicate grade assignments surface as an explicit error.
	if grade != nil {
		var current *string
		if err := s.db.QueryRow(c.Context(),
			`SELECT grade FROM players WHERE id = $1`, id).Scan(&current); err != nil {
			return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Player not found.")
		}
		if current != nil && *current == *grade {
			return httpx.WriteAppError(c, httpx.Conflict("GRADE_EXISTS",
				"Player already has grade "+*grade+"."))
		}
	}
	var p Player
	err := s.db.QueryRow(c.Context(),
		`UPDATE players SET name = $2, phone = $3, notes = $4,
		 status = COALESCE($5, status), grade = COALESCE($6, grade), gender = COALESCE($7, gender), updated_at = now()
		 WHERE id = $1
		 RETURNING id::text, name, phone, notes, status, grade, gender, created_at::text`,
		id, strings.TrimSpace(*in.Name), in.Phone, in.Notes, in.Status, grade, gender,
	).Scan(&p.ID, &p.Name, &p.Phone, &p.Notes, &p.Status, &p.Grade, &p.Gender, &p.CreatedAt)
	if err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Player not found.")
	}
	return httpx.OK(c, http.StatusOK, p)
}

// Delete hard-deletes a player, but only when nothing references them:
// attendance, matches, bills, payments, revenues, memberships, or recap
// stats. A player with any history is refused so money trails and past
// sessions stay intact.
func (s *Service) Delete(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid player ID.")
	}
	var refs int
	_ = s.db.QueryRow(c.Context(), `
		SELECT (SELECT COUNT(*) FROM attendances WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM match_players WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM player_bills WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM payments WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM revenues WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM memberships WHERE player_id = $1)
		     + (SELECT COUNT(*) FROM session_player_stats WHERE player_id = $1)`,
		id).Scan(&refs)
	if refs > 0 {
		return httpx.WriteAppError(c, httpx.Conflict("PLAYER_HAS_HISTORY",
			"Player already has attendance, bills, or other history and cannot be deleted."))
	}
	tag, err := s.db.Exec(c.Context(), `DELETE FROM players WHERE id = $1`, id)
	if err != nil || tag.RowsAffected() == 0 {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "Player not found.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"id": id, "deleted": true})
}
