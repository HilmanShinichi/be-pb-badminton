package httpx

import (
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	UserID   string `json:"uid"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
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
		tokenStr := ""
		header := c.Get("Authorization")
		if strings.HasPrefix(header, "Bearer ") {
			tokenStr = strings.TrimPrefix(header, "Bearer ")
		} else if qToken := c.Query("token"); qToken != "" {
			tokenStr = qToken
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
