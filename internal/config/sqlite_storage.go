package config

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// SQLiteStorageConfig controls connection-local SQLite contention handling.
type SQLiteStorageConfig struct {
	BusyTimeout string `toml:"busy_timeout"`
}

// BusyTimeoutDuration defaults to 5s, matching SQLite's existing busy handler.
// Millisecond precision avoids silently truncating a configured duration.
func (c SQLiteStorageConfig) BusyTimeoutDuration() (time.Duration, error) {
	raw := strings.TrimSpace(c.BusyTimeout)
	if raw == "" {
		return 5 * time.Second, nil
	}
	wait, err := time.ParseDuration(raw)
	if err != nil || wait < time.Millisecond || wait > 5*time.Minute || wait%time.Millisecond != 0 {
		return 0, fmt.Errorf("storage.sqlite.busy_timeout must be a whole-millisecond duration between 1ms and 5m: %q", raw)
	}
	return wait, nil
}

func applySQLiteStorageEnv(cfg *SQLiteStorageConfig) {
	if raw := strings.TrimSpace(os.Getenv("KATA_SQLITE_BUSY_TIMEOUT")); raw != "" {
		cfg.BusyTimeout = raw
	}
}

// KataSQLiteStorageConfig uses the storage-only reader so direct database
// operations do not depend on unrelated daemon settings.
func KataSQLiteStorageConfig(ctx context.Context) (SQLiteStorageConfig, error) {
	_ = ctx
	storage, err := readStorageConfig()
	if err != nil {
		return SQLiteStorageConfig{}, err
	}
	applySQLiteStorageEnv(&storage.SQLite)
	if _, err := storage.SQLite.BusyTimeoutDuration(); err != nil {
		return SQLiteStorageConfig{}, err
	}
	return storage.SQLite, nil
}
