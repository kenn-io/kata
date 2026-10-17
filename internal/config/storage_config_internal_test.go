package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
)

// A valid file's unused web origin cannot change TUI preference resolution,
// even when the origin is semantically invalid. Encoding keeps fuzz inputs
// within valid TOML so parse failures cannot mask a validation-scope bug.
func FuzzReadConfigSectionsIgnoresUnusedWeb(f *testing.F) {
	f.Add("http://daemon.example", true)
	f.Add("not an origin", false)
	f.Add("https://daemon.example/path", true)
	home := f.TempDir()
	f.Setenv("KATA_HOME", home)
	f.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	f.Fuzz(func(t *testing.T, origin string, mouse bool) {
		if !utf8.ValidString(origin) {
			t.Skip("TOML strings require UTF-8")
		}
		var data bytes.Buffer
		require.NoError(t, toml.NewEncoder(&data).Encode(map[string]any{
			"web": map[string]any{"public_origin": origin},
			"tui": map[string]any{"mouse": mouse},
		}))
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), data.Bytes(), 0600))
		cfg, err := ReadConfigSections("tui")
		require.NoError(t, err)
		require.Equal(t, mouse, cfg.TUI.Mouse)
	})
}
