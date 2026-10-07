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
	"github.com/google/uuid"
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
	var isSuper bool
	var perms []string
	err := s.db.QueryRow(c.Context(),
		`SELECT id::text, password_hash, COALESCE(is_superadmin, FALSE), COALESCE(permissions, '{}') FROM users WHERE username = $1`, req.Username,
	).Scan(&id, &hash, &isSuper, &perms)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		s.noteLoginFailure(c.IP(), user)
		return httpx.Err(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Incorrect username or password.")
	}
	s.resetLoginFailures(user)

	role := "ADMIN"
	if isSuper {
		role = "SUPERADMIN"
	}
	claims := &httpx.Claims{
		UserID: id, Username: req.Username, Role: role,
		Permissions: httpx.NormalizePerms(perms), IsSuperadmin: isSuper,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour))},
	}
	token, err := httpx.SignToken(s.jwtSecret, claims)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create session.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"token": token, "username": req.Username, "role": role,
		"permissions": claims.Permissions, "is_superadmin": isSuper})
}

func (s *Service) Me(c *fiber.Ctx) error {
	cl := httpx.ClaimsFrom(c)
	return httpx.OK(c, http.StatusOK, map[string]any{"username": cl.Username, "role": cl.Role,
		"permissions": cl.Permissions, "is_superadmin": cl.IsSuperadmin})
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

type userRow struct {
	ID           string   `json:"id"`
	Username     string   `json:"username"`
	IsSuperadmin bool     `json:"is_superadmin"`
	Permissions  []string `json:"permissions"`
	CreatedAt    string   `json:"created_at"`
}

func scanUserRow(row interface {
	Scan(...any) error
}) (userRow, error) {
	var u userRow
	var perms []string
	if err := row.Scan(&u.ID, &u.Username, &u.IsSuperadmin, &perms, &u.CreatedAt); err != nil {
		return u, err
	}
	u.Permissions = httpx.NormalizePerms(perms)
	if u.Permissions == nil {
		u.Permissions = []string{}
	}
	return u, nil
}

// ListUsers returns every admin account. Superadmins only.
func (s *Service) ListUsers(c *fiber.Ctx) error {
	rows, err := s.db.Query(c.Context(),
		`SELECT id::text, username, COALESCE(is_superadmin, FALSE), COALESCE(permissions, '{}'), created_at::text
		 FROM users ORDER BY username`)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not load users.")
	}
	defer rows.Close()
	users := []userRow{}
	for rows.Next() {
		if u, err := scanUserRow(rows); err == nil {
			users = append(users, u)
		}
	}
	return httpx.OK(c, http.StatusOK, users)
}

// CreateUser opens a new admin account with an explicit permission
// allowlist (or all-access via is_superadmin). Superadmins only.
func (s *Service) CreateUser(c *fiber.Ctx) error {
	var in struct {
		Username     string   `json:"username"`
		Password     string   `json:"password"`
		Permissions  []string `json:"permissions"`
		IsSuperadmin bool     `json:"is_superadmin"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	username := strings.ToLower(strings.TrimSpace(in.Username))
	if len(username) < 3 || len(username) > 32 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Username must be 3-32 characters."))
	}
	if len(in.Password) < minPasswordLen {
		return httpx.WriteAppError(c, httpx.Unprocessable("Password must be at least 8 characters."))
	}
	perms := httpx.NormalizePerms(in.Permissions)
	if !in.IsSuperadmin && len(perms) == 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Pick at least one accessible feature, or grant all access."))
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create user.")
	}
	var u userRow
	err = s.db.QueryRow(c.Context(), `
		INSERT INTO users (id, username, password_hash, is_superadmin, permissions)
		VALUES (gen_random_uuid(), $1, $2, $3, $4)
		RETURNING id::text, username, is_superadmin, permissions, created_at::text`,
		username, string(hash), in.IsSuperadmin, perms).Scan(&u.ID, &u.Username, &u.IsSuperadmin, &u.Permissions, &u.CreatedAt)
	if err != nil {
		return httpx.WriteAppError(c, httpx.Conflict("USERNAME_TAKEN", "That username is already taken."))
	}
	u.Permissions = httpx.NormalizePerms(u.Permissions)
	return httpx.OK(c, http.StatusCreated, u)
}

// UpdateUser changes permissions, superadmin flag, and/or password.
// Self-modification of access is forbidden (use change-password for self);
// the last superadmin can neither be demoted nor deleted.
func (s *Service) UpdateUser(c *fiber.Ctx) error {
	cl := httpx.ClaimsFrom(c)
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid user ID."))
	}
	var target userRow
	if err := s.db.QueryRow(c.Context(),
		`SELECT id::text, username, COALESCE(is_superadmin, FALSE), COALESCE(permissions, '{}'), created_at::text
		 FROM users WHERE id = $1`, id).Scan(&target.ID, &target.Username, &target.IsSuperadmin, &target.Permissions, &target.CreatedAt); err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "User not found.")
	}
	var in struct {
		Permissions  *[]string `json:"permissions"`
		IsSuperadmin *bool     `json:"is_superadmin"`
		Password     *string   `json:"password"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		return httpx.Err(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request body.")
	}
	if target.ID == cl.UserID && (in.Permissions != nil || in.IsSuperadmin != nil) {
		return httpx.WriteAppError(c, httpx.Unprocessable("You cannot change your own access. Ask another superadmin."))
	}
	newSuper := target.IsSuperadmin
	if in.IsSuperadmin != nil {
		newSuper = *in.IsSuperadmin
	}
	newPerms := httpx.NormalizePerms(target.Permissions)
	if in.Permissions != nil {
		newPerms = httpx.NormalizePerms(*in.Permissions)
	}
	if !newSuper && len(newPerms) == 0 {
		return httpx.WriteAppError(c, httpx.Unprocessable("Pick at least one accessible feature, or grant all access."))
	}
	if target.IsSuperadmin && !newSuper {
		var others int
		if err := s.db.QueryRow(c.Context(),
			`SELECT COUNT(*) FROM users WHERE is_superadmin AND id <> $1`, id).Scan(&others); err != nil || others == 0 {
			return httpx.WriteAppError(c, httpx.Conflict("LAST_SUPERADMIN", "The last superadmin cannot be demoted."))
		}
	}
	setHash := ""
	if in.Password != nil && *in.Password != "" {
		if len(*in.Password) < minPasswordLen {
			return httpx.WriteAppError(c, httpx.Unprocessable("Password must be at least 8 characters."))
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*in.Password), bcrypt.DefaultCost)
		if err != nil {
			return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update user.")
		}
		setHash = string(hash)
	}
	if _, err := s.db.Exec(c.Context(), `
		UPDATE users SET is_superadmin = $2, permissions = $3,
		    password_hash = CASE WHEN $4 = '' THEN password_hash ELSE $4 END,
		    updated_at = now()
		WHERE id = $1`, id, newSuper, newPerms, setHash); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not update user.")
	}
	target.IsSuperadmin, target.Permissions = newSuper, newPerms
	return httpx.OK(c, http.StatusOK, target)
}

// DeleteUser removes an account. Self-deletion and deleting the last
// superadmin are refused.
func (s *Service) DeleteUser(c *fiber.Ctx) error {
	cl := httpx.ClaimsFrom(c)
	id := c.Params("id")
	if _, err := uuid.Parse(id); err != nil {
		return httpx.WriteAppError(c, httpx.BadRequest("BAD_REQUEST", "Invalid user ID."))
	}
	if id == cl.UserID {
		return httpx.WriteAppError(c, httpx.Unprocessable("You cannot delete your own account."))
	}
	var isSuper bool
	if err := s.db.QueryRow(c.Context(),
		`SELECT COALESCE(is_superadmin, FALSE) FROM users WHERE id = $1`, id).Scan(&isSuper); err != nil {
		return httpx.Err(c, http.StatusNotFound, "NOT_FOUND", "User not found.")
	}
	if isSuper {
		var others int
		if err := s.db.QueryRow(c.Context(),
			`SELECT COUNT(*) FROM users WHERE is_superadmin AND id <> $1`, id).Scan(&others); err != nil || others == 0 {
			return httpx.WriteAppError(c, httpx.Conflict("LAST_SUPERADMIN", "The last superadmin cannot be deleted."))
		}
	}
	if _, err := s.db.Exec(c.Context(), `DELETE FROM users WHERE id = $1`, id); err != nil {
		return httpx.Err(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not delete user.")
	}
	return httpx.OK(c, http.StatusOK, map[string]any{"ok": true})
}

// ChangePassword rotates the caller's own password. It exists so the seeded
// default credential can be retired without database access.
func (s *Service) ChangePassword(c *fiber.Ctx) error {	cl := httpx.ClaimsFrom(c)
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

