package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

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

func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decode(r, &req); err != nil {
		httpx.Err(w, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
		return
	}
	if req.Username == "" || req.Password == "" {
		httpx.Err(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Username and password are required.")
		return
	}

	var id, hash string
	err := s.db.QueryRow(r.Context(),
		`SELECT id::text, password_hash FROM users WHERE username = $1`, req.Username,
	).Scan(&id, &hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		httpx.Err(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Incorrect username or password.")
		return
	}

	claims := &httpx.Claims{
		UserID: id, Username: req.Username, Role: "ADMIN",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour))},
	}
	token, err := httpx.SignToken(s.jwtSecret, claims)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create session.")
		return
	}
	httpx.OK(w, http.StatusOK, map[string]any{"token": token, "username": req.Username, "role": "ADMIN"})
}

func (s *Service) Me(w http.ResponseWriter, r *http.Request) {
	c := httpx.ClaimsFrom(r.Context())
	httpx.OK(w, http.StatusOK, map[string]any{"username": c.Username, "role": c.Role})
}

func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, http.StatusOK, map[string]any{"ok": true})
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

func decode(r *http.Request, into any) error {
	return httpx.Decode(r, into)
}
