package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSQLiteBusyTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		name, file, env string
		valid           bool
	}{
		{"toml", "1s", "", true},
		{"env overrides invalid file", "invalid", "2s", true},
		{"zero", "0s", "", false},
		{"negative", "-1s", "", false},
		{"too large", "301s", "", false},
		{"fractional millisecond", "1.5ms", "", false},
		{"invalid env", "1s", "bad", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			t.Setenv("KATA_SQLITE_BUSY_TIMEOUT", tc.env)
			if tc.file != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[storage.sqlite]\nbusy_timeout = \""+tc.file+"\"\n"), 0600))
			}
			_, err := ReadDaemonConfig()
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "storage.sqlite.busy_timeout")
			}
		})
	}
}

func TestSQLiteBusyTimeoutDuration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", raw: "", want: 5 * time.Second},
		{name: "minimum", raw: "1ms", want: time.Millisecond},
		{name: "maximum", raw: "5m", want: 5 * time.Minute},
		{name: "below minimum", raw: "999us", wantErr: true},
		{name: "above maximum", raw: "5m1ms", wantErr: true},
		{name: "zero", raw: "0s", wantErr: true},
		{name: "negative", raw: "-1ms", wantErr: true},
		{name: "fractional millisecond", raw: "1.5ms", wantErr: true},
		{name: "invalid", raw: "bad", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (SQLiteStorageConfig{BusyTimeout: tc.raw}).BusyTimeoutDuration()
			if tc.wantErr {
				require.ErrorContains(t, err, "storage.sqlite.busy_timeout")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
