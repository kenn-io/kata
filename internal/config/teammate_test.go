package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestReadTeammateConfig(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		invalid          bool
	}{
		{"missing", "", "model", false}, {"empty section", "[teammate]", "model", false},
		{"model", "[teammate]\ninfer='model'", "model", false}, {"harness", "[teammate]\ninfer='harness'", "harness", false}, {"off", "[teammate]\ninfer='off'", "off", false},
		{"empty", "[teammate]\ninfer=''", "", true}, {"unknown mode", "[teammate]\ninfer='maybe'", "", true}, {"wrong type", "[teammate]\ninfer=1", "", true}, {"unknown key", "[teammate]\nifner='model'", "", true},
		{"unrelated invalid", "[auth]\ntoken_file='/missing/secret'", "model", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			if tc.text != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.text), 0600))
			}
			got, err := config.ReadTeammateConfig()
			if tc.invalid {
				require.Error(t, err)
				require.Contains(t, err.Error(), "teammate")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Infer)
		})
	}
}
