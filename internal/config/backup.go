package config

import (
	"fmt"
	"strings"
	"time"
)

// BackupConfig enables daemon-owned JSONL snapshots with time-based retention.
// An empty section disables scheduling. Dir enables 24h/720h defaults.
type BackupConfig struct {
	Dir      string `toml:"dir"`
	Interval string `toml:"interval"`
	Retain   string `toml:"retain"`
}

// Durations validates an enabled schedule or rejects partial configuration.
func (c BackupConfig) Durations() (interval, retain time.Duration, err error) {
	if strings.TrimSpace(c.Dir) == "" {
		if c.Interval != "" || c.Retain != "" {
			return 0, 0, fmt.Errorf("backup.dir is required when interval or retain is configured")
		}
		return 0, 0, nil
	}
	interval, retain = 24*time.Hour, 30*24*time.Hour
	for name, raw := range map[string]string{"interval": c.Interval, "retain": c.Retain} {
		if raw == "" {
			continue
		}
		duration, parseErr := time.ParseDuration(strings.TrimSpace(raw))
		if parseErr != nil || duration <= 0 {
			return 0, 0, fmt.Errorf("backup.%s must be a positive duration", name)
		}
		if name == "interval" {
			interval = duration
		} else {
			retain = duration
		}
	}
	return interval, retain, nil
}
