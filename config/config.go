package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	BackupPath       string
	SessionDBPath    string
	GroupAllowlist   []string
	GroupDenylist    []string
	MediaWorkers     int
	LogLevel         string
	HistorySyncIdle  time.Duration
}

func Load() (*Config, error) {
	// .env is loaded first; explicit OS env vars take precedence because
	// godotenv.Load does not overwrite existing variables.
	_ = godotenv.Load()

	workers, err := strconv.Atoi(getenv("MEDIA_WORKERS", "4"))
	if err != nil || workers <= 0 {
		return nil, fmt.Errorf("MEDIA_WORKERS must be a positive integer, got %q", os.Getenv("MEDIA_WORKERS"))
	}

	idle, err := time.ParseDuration(getenv("HISTORY_SYNC_IDLE", "30s"))
	if err != nil {
		return nil, fmt.Errorf("HISTORY_SYNC_IDLE invalid duration: %w", err)
	}

	level := strings.ToLower(getenv("LOG_LEVEL", "info"))
	switch level {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("LOG_LEVEL must be debug|info|warn|error, got %q", level)
	}

	cfg := &Config{
		BackupPath:      getenv("LOCAL_BACKUP_PATH", "./backup"),
		SessionDBPath:   getenv("SESSION_DB_PATH", "./wa-session.db"),
		GroupAllowlist:  splitCSV(os.Getenv("GROUP_ALLOWLIST")),
		GroupDenylist:   splitCSV(os.Getenv("GROUP_DENYLIST")),
		MediaWorkers:    workers,
		LogLevel:        level,
		HistorySyncIdle: idle,
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
