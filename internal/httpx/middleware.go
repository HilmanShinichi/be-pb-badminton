package httpx

import (
	"context"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

type ctxKey int

const userIDKey ctxKey = iota

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

func RequireAuth(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenStr := ""
			header := r.Header.Get("Authorization")
			if strings.HasPrefix(header, "Bearer ") {
				tokenStr = strings.TrimPrefix(header, "Bearer ")
			} else if qToken := r.URL.Query().Get("token"); qToken != "" {
				tokenStr = qToken
			}
			if tokenStr == "" {
				Err(w, http.StatusUnauthorized, "UNAUTHORIZED", "Login is required.")
				return
			}
			claims, err := ParseToken(secret, tokenStr)
			if err != nil {
				Err(w, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid session.")
				return
			}
			ctx := context.WithValue(r.Context(), userIDKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func ClaimsFrom(ctx context.Context) *Claims {
	if c, ok := ctx.Value(userIDKey).(*Claims); ok {
		return c
	}
	return &Claims{}
}
