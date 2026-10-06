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
	// Fallback chain: AI 2 dipakai kalau AI 1 gagal (provider error /
	// rate-limit / tidak bisa dihubungi), AI 3 kalau AI 2 juga gagal.
	AIProvider2 string
	AIModel2    string
	AIBaseURL2  string
	AIAPIKey2   string
	AIProvider3 string
	AIModel3    string
	AIBaseURL3  string
	AIAPIKey3   string
	// AIDebug logs truncated raw AI responses (AI_DEBUG=1). For diagnosing
	// why an endpoint's answer was unusable. Off by default.
	AIDebug bool
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
		AIProvider2:     env("AI_PROVIDER_2", ""),
		AIModel2:        env("AI_MODEL_2", ""),
		AIBaseURL2:      env("AI_BASE_URL_2", ""),
		AIAPIKey2:       env("AI_API_KEY_2", ""),
		AIProvider3:     env("AI_PROVIDER_3", ""),
		AIModel3:        env("AI_MODEL_3", ""),
		AIBaseURL3:      env("AI_BASE_URL_3", ""),
		AIAPIKey3:       env("AI_API_KEY_3", ""),
		AIDebug:         envBool("AI_DEBUG", false),
	}
}

func (c Config) DatabaseConnectionString() string {
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		return dbURL
	}
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

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
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
