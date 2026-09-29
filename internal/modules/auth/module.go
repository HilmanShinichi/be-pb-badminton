package auth

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
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

	// In-memory brute-force guard for Login. Keyed by account ("user:<name>")
	// and by network ("ip:<addr>") so one attacker cannot lock out unrelated
	// users and rotating usernames does not bypass the IP budget either.
	// NOTE: c.IP() is the direct peer address (no ProxyHeader configured);
	// behind a reverse proxy all clients may share one IP, in which case the
	// per-account bucket is the binding one.
	mu         sync.Mutex
	attempts   map[string]*loginAttempt
	lastSweep  time.Time
}

const (
	// Per account: 5 wrong passwords in 10 minutes locks that account for
	// 5 minutes. Per IP: 30 failures in 10 minutes locks the network peer.
	loginUserFails  = 5
	loginIPFails    = 30
	loginWindow     = 10 * time.Minute
	loginBlockFor   = 5 * time.Minute
	minPasswordLen  = 8
)

type loginAttempt struct {
	fails        []time.Time
	blockedUntil time.Time
}

func NewService(db *pgxpool.Pool, jwtSecret string) *Service {
	return &Service{db: db, jwtSecret: jwtSecret, attempts: map[string]*loginAttempt{}}
}

// sweepLocked drops expired buckets. Callers must hold s.mu.
func (s *Service) sweepLocked(now time.Time) {
	if now.Sub(s.lastSweep) < time.Minute {
		return
	}
	s.lastSweep = now
	for k, a := range s.attempts {
		kept := a.fails[:0]
		for _, t := range a.fails {
			if now.Sub(t) < loginWindow {
				kept = append(kept, t)
			}
		}
		a.fails = kept
		if len(kept) == 0 && now.After(a.blockedUntil) {
			delete(s.attempts, k)
		}
	}
}

// loginBlocked reports whether this login must be refused right now.
func (s *Service) loginBlocked(ip, user string) (blocked bool, retryAfter time.Duration) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	for _, k := range []string{"user:" + user, "ip:" + ip} {
		if a, ok := s.attempts[k]; ok && now.Before(a.blockedUntil) {
			return true, time.Until(a.blockedUntil).Round(time.Second)
		}
	}
	return false, 0
}

// noteLoginFailure records one wrong password and arms a block when either
// budget is spent.
func (s *Service) noteLoginFailure(ip, user string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	record := func(key string, limit int) {
		a, ok := s.attempts[key]
		if !ok {
			a = &loginAttempt{}
			s.attempts[key] = a
		}
		a.fails = append(a.fails, now)
		if len(a.fails) >= limit {
			a.blockedUntil = now.Add(loginBlockFor)
		}
	}
	record("user:"+user, loginUserFails)
	record("ip:"+ip, loginIPFails)
}

// resetLoginFailures clears the account bucket after a successful login.
func (s *Service) resetLoginFailures(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attempts, "user:"+user)
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
	user := strings.ToLower(strings.TrimSpace(req.Username))
	if blocked, retryAfter := s.loginBlocked(c.IP(), user); blocked {
		secs := int(retryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		c.Set("Retry-After", strconv.Itoa(secs))
		return httpx.Err(c, http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "Too many login attempts. Try again in a few minutes.")
	}

	var id, hash string
	err := s.db.QueryRow(c.Context(),
		`SELECT id::text, password_hash FROM users WHERE username = $1`, req.Username,
	).Scan(&id, &hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		s.noteLoginFailure(c.IP(), user)
		return httpx.Err(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Incorrect username or password.")
	}
	s.resetLoginFailures(user)

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
// database is reachable without manual seeding. The seed password comes from
// ADMIN_DEFAULT_PASSWORD; leaving it unset keeps the well-known "admin"
// fallback and logs a loud warning, so production is pushed to set it.
func (s *Service) EnsureAdminUser(ctx context.Context) error {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	seed := strings.TrimSpace(os.Getenv("ADMIN_DEFAULT_PASSWORD"))
	if seed == "" {
		seed = "admin"
		log.Printf("WARNING: seeding default admin with the well-known password. Set ADMIN_DEFAULT_PASSWORD and change it after first login.")
	}
	if len(seed) < minPasswordLen {
		log.Printf("WARNING: ADMIN_DEFAULT_PASSWORD is shorter than %d characters.", minPasswordLen)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(seed), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES (gen_random_uuid(), 'admin', $1)`, string(hash))
	return err
}

// ChangePassword rotates the caller's own password. It exists so the seeded
// default credential can be retired without database access.
func (s *Service) ChangePassword(c *fiber.Ctx) error {
	cl := httpx.ClaimsFrom(c)
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if in.CurrentPassword == "" || in.NewPassword == "" {
		return httpx.Err(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Current and new passwords are required.")
	}
	if len(in.NewPassword) < minPasswordLen {
		return httpx.WriteAppError(c, httpx.Unprocessable("New password must be at least 8 characters."))
	}
	var hash string
	if err := s.db.QueryRow(c.Context(),
		`SELECT password_hash FROM users WHERE id = $1`, cl.UserID).Scan(&hash); err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "User not found.")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.CurrentPassword)) != nil {
		return httpx.Err(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Current password is incorrect.")
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update password.")
	}
	if _, err := s.db.Exec(c.Context(),
		`UPDATE users SET password_hash = $1 WHERE id = $2`, string(newHash), cl.UserID); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update password.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

