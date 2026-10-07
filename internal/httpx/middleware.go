package httpx

import (
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	UserID       string   `json:"uid"`
	Username     string   `json:"username"`
	Role         string   `json:"role"`
	Permissions  []string `json:"perms"`
	IsSuperadmin bool     `json:"super"`
	jwt.RegisteredClaims
}

// Feature permission keys. users management itself is superadmin-only and
// has no grantable key.
const (
	PermDashboard  = "dashboard"
	PermMabar      = "mabar"
	PermPlayers    = "players"
	PermPeriods    = "periods"
	PermMatchmaker = "matchmaker"
	PermInventory  = "inventory"
	PermFinance    = "finance"
	PermReports    = "reports"
	PermSimulator  = "simulator"
)

// ValidPerms is the grantable allowlist for non-superadmin accounts.
var ValidPerms = map[string]bool{
	PermDashboard: true, PermMabar: true, PermPlayers: true,
	PermPeriods: true, PermMatchmaker: true, PermInventory: true,
	PermFinance: true, PermReports: true, PermSimulator: true,
}

// NormalizePerms lowercases, dedupes, and drops unknown keys.
func NormalizePerms(perms []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range perms {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || !ValidPerms[p] || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Can reports whether these claims may use a feature. Superadmins bypass
// every check; tokens minted before RBAC carry no permissions and must
// re-login.
func (c *Claims) Can(perm string) bool {
	if c == nil {
		return false
	}
	if c.IsSuperadmin {
		return true
	}
	for _, p := range c.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// RequirePerm forbids the request when the caller lacks one feature.
func RequirePerm(perm string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if ClaimsFrom(c).Can(perm) {
			return c.Next()
		}
		return Err(c, http.StatusForbidden, "FORBIDDEN", "You don't have access to this feature.")
	}
}

// RequireAnyPerm forbids unless the caller has at least one listed feature.
// Used for read endpoints one feature needs from another (e.g. match maker
// needs the player directory and session attendance to build draws).
func RequireAnyPerm(perms ...string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		cl := ClaimsFrom(c)
		for _, p := range perms {
			if cl.Can(p) {
				return c.Next()
			}
		}
		return Err(c, http.StatusForbidden, "FORBIDDEN", "You don't have access to this feature.")
	}
}

// RequireSuperadmin locks user management to superadmins.
func RequireSuperadmin() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if cl := ClaimsFrom(c); cl != nil && cl.IsSuperadmin {
			return c.Next()
		}
		return Err(c, http.StatusForbidden, "FORBIDDEN", "Only superadmins can manage users.")
	}
}

func SignToken(secret string, claims *Claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func ParseToken(secret, raw string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		return nil, Unauthorized("Invalid session.")
	}
	return token.Claims.(*Claims), nil
}

func RequireAuth(secret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Tokens travel in the Authorization header only. A previous
		// ?token= query fallback was removed so session tokens never leak
		// into browser history, bookmarks, or server access logs (CSV
		// exports now download via an authenticated fetch instead).
		tokenStr := ""
		if header := c.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
			tokenStr = strings.TrimPrefix(header, "Bearer ")
		}
		if tokenStr == "" {
			return Err(c, http.StatusUnauthorized, "UNAUTHORIZED", "Login is required.")
		}
		claims, err := ParseToken(secret, tokenStr)
		if err != nil {
			return Err(c, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid session.")
		}
		c.Locals("claims", claims)
		return c.Next()
	}
}

func ClaimsFrom(c *fiber.Ctx) *Claims {
	if cl, ok := c.Locals("claims").(*Claims); ok {
		return cl
	}
	return &Claims{}
}
