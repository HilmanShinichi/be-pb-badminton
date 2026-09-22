package config

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port            string
	DBHost          string
	DBPort          string
	DBName          string
	DBUser          string
	DBPassword      string
	DBSSLMode       string
	JWTSecret       string
	CORSOrigins     string
	InactivityMonth int
	AIProvider      string
	AIModel         string
	AIBaseURL       string
	AIAPIKey        string
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
		DBHost:          env("DB_HOST", "localhost"),
		DBPort:          env("DB_PORT", "5432"),
		DBName:          env("DB_NAME", "pb_kecebong"),
		DBUser:          env("DB_USER", "pb"),
		DBPassword:      env("DB_PASSWORD", "pb"),
		DBSSLMode:       env("DB_SSLMODE", "disable"),
		JWTSecret:       env("JWT_SECRET", "dev-only-secret"),
		CORSOrigins:     env("CORS_ORIGINS", "http://localhost:5173"),
		InactivityMonth: envInt("INACTIVITY_MONTHS", 6),
		AIProvider:      env("AI_PROVIDER", "openai"),
		AIModel:         env("AI_MODEL", ""),
		AIBaseURL:       env("AI_BASE_URL", ""),
		AIAPIKey:        env("AI_API_KEY", ""),
	}
}

func (c Config) DatabaseConnectionString() string {
	databaseURL := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(c.DBHost, c.DBPort),
		Path:   c.DBName,
		User:   url.UserPassword(c.DBUser, c.DBPassword),
	}
	query := databaseURL.Query()
	query.Set("sslmode", c.DBSSLMode)
	databaseURL.RawQuery = query.Encode()
	return databaseURL.String()
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
