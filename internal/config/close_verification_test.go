package config

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestCloseVerificationConfig(t *testing.T) {
	for _, value := range []string{"", "block", "warn", "off", "invalid"} {
		t.Run(value, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.verification]\nopen_refutations = \""+value+"\"\n"), 0600))
			cfg, err := ReadDaemonConfig()
			if value == "invalid" {
				require.ErrorContains(t, err, "close.verification.open_refutations")
				return
			}
			require.NoError(t, err)
			want := value
			if want == "" {
				want = "warn"
			}
			require.Equal(t, want, cfg.Close.Verification.Policy())
		})
	}
}
