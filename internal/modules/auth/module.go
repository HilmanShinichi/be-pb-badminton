package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/gofiber/fiber/v2"

	"github.com/pb-kecebong/backend/internal/httpx"
)

type Service struct {
	db        *pgxpool.Pool
	jwtSecret string
}

func NewService(db *pgxpool.Pool, jwtSecret string) *Service {
	return &Service{db: db, jwtSecret: jwtSecret}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Service) Login(c *fiber.Ctx) error {
	var req loginRequest
	if err := httpx.Decode(c, &req); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if req.Username == "" || req.Password == "" {
		return httpx.Err(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Username and password are required.")
	}

	var id, hash string
	err := s.db.QueryRow(c.Context(),
		`SELECT id::text, password_hash FROM users WHERE username = $1`, req.Username,
	).Scan(&id, &hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		return httpx.Err(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Incorrect username or password.")
	}

	claims := &httpx.Claims{
		UserID: id, Username: req.Username, Role: "ADMIN",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour))},
	}
	token, err := httpx.SignToken(s.jwtSecret, claims)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create session.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"token": token, "username": req.Username, "role": "ADMIN"})
}

func (s *Service) Me(c *fiber.Ctx) error {
	cl := httpx.ClaimsFrom(c)
	return httpx.OK(c, http.StatusOK, map[string]any{"username": cl.Username, "role": cl.Role})
}

func (s *Service) Logout(c *fiber.Ctx) error {
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

// EnsureAdminUser creates the default admin if no user exists, so a fresh
// database is reachable without manual seeding.
func (s *Service) EnsureAdminUser(ctx context.Context) error {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES (gen_random_uuid(), 'admin', $1)`, string(hash))
	return err
}

