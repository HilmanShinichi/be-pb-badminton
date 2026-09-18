package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port            string
	DatabaseURL     string
	JWTSecret       string
	CORSOrigins     string
	InactivityMonth int
}

// LoadDotEnv reads KEY=VALUE lines from path into the environment. Existing
// variables win, so a real environment always overrides the file.
func LoadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, val, _ := strings.Cut(line, "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func Load() Config {
	return Config{
		Port:            env("PORT", "8080"),
		DatabaseURL:     env("DATABASE_URL", "postgres://pb:pb@localhost:5432/pb_kecebong?sslmode=disable"),
		JWTSecret:       env("JWT_SECRET", "dev-only-secret"),
		CORSOrigins:     env("CORS_ORIGINS", "http://localhost:5173"),
		InactivityMonth: envInt("INACTIVITY_MONTHS", 6),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
