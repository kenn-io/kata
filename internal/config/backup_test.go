package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// Contract: backup env replaces TOML, durations are positive, and a schedule
// without a destination is rejected before daemon startup.
func TestDeploymentBackupConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, body, interval, retain string
		wantError                    bool
	}{
		{name: "disabled"},
		{name: "defaults", body: "[backup]\ndir = %q\n", interval: "24h", retain: "720h"},
		{name: "explicit", body: "[backup]\ndir = %q\ninterval = \"2h\"\nretain = \"48h\"\n", interval: "2h", retain: "48h"},
		{name: "destination required", body: "[backup]\ninterval = \"1h\"\n", wantError: true},
		{name: "invalid interval", body: "[backup]\ndir = %q\ninterval = \"bad\"\n", wantError: true},
		{name: "zero interval", body: "[backup]\ndir = %q\ninterval = \"0s\"\n", wantError: true},
		{name: "negative retention", body: "[backup]\ndir = %q\nretain = \"-1h\"\n", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			body := tc.body
			if tc.interval != "" || (tc.wantError && tc.name != "destination required") {
				body = fmt.Sprintf(body, filepath.Join(home, "backups"))
			}
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
			cfg, err := config.ReadDaemonConfig()
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.interval == "" {
				require.Empty(t, cfg.Backup.Dir)
				return
			}
			interval, retain, err := cfg.Backup.Durations()
			require.NoError(t, err)
			wantInterval, err := time.ParseDuration(tc.interval)
			require.NoError(t, err)
			wantRetain, err := time.ParseDuration(tc.retain)
			require.NoError(t, err)
			require.Equal(t, wantInterval, interval)
			require.Equal(t, wantRetain, retain)
			t.Setenv("KATA_BACKUP_DIR", filepath.Join(home, "env-backups"))
			t.Setenv("KATA_BACKUP_INTERVAL", "3h")
			t.Setenv("KATA_BACKUP_RETAIN", "72h")
			cfg, err = config.ReadDaemonConfig()
			require.NoError(t, err)
			require.Equal(t, filepath.Join(home, "env-backups"), cfg.Backup.Dir)
			interval, retain, err = cfg.Backup.Durations()
			require.NoError(t, err)
			require.Equal(t, 3*time.Hour, interval)
			require.Equal(t, 72*time.Hour, retain)
		})
	}
}
