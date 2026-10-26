package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestReadCreateConfig(t *testing.T) {
	cases := []struct {
		name, text string
		want       *int64
		wantErr    string
		missing    bool
		directory  bool
	}{
		{name: "missing file", missing: true},
		{name: "config is directory", directory: true, wantErr: "read"},
		{name: "empty section", text: "[create]"},
		{name: "unrelated settings", text: "[auth]\ntoken_file = '/missing/credential'\n[web]\npublic_origin = 'invalid'"},
		{name: "zero", text: "[create]\ndefault_priority = 0", want: new(int64(0))},
		{name: "quoted unrelated key", text: "\"create.default_priority\" = 4", want: nil},
		{name: "four", text: "[create]\ndefault_priority = 4", want: new(int64(4))},
		{name: "negative", text: "[create]\ndefault_priority = -1", wantErr: "create.default_priority"},
		{name: "too high", text: "[create]\ndefault_priority = 5", wantErr: "create.default_priority"},
		{name: "string", text: "[create]\ndefault_priority = '3'", wantErr: "create.default_priority"},
		{name: "unknown", text: "[create]\ndefaut_priority = 3", wantErr: "create.defaut_priority"},
		{name: "unknown nested", text: "[create.future]\nenabled = true", wantErr: "create.future"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			path := filepath.Join(home, "config.toml")
			if tc.directory {
				require.NoError(t, os.Mkdir(path, 0o700))
			} else if !tc.missing {
				require.NoError(t, os.WriteFile(path, []byte(tc.text), 0o600))
			}
			got, err := config.ReadCreateConfig()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), filepath.Join(home, "config.toml"))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.DefaultPriority)
		})
	}
}
