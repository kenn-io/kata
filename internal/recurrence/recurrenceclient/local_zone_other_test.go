//go:build !windows

package recurrenceclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalZoneUsesEnvironmentAndLocaltimeLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "localtime")
	require.NoError(t, os.Symlink("/usr/share/zoneinfo/Asia/Tokyo", path))
	t.Setenv("TZ", "")
	assert.Equal(t, "UTC", LocalZone(path), "TZ set but empty means UTC, not the localtime link")
	require.NoError(t, os.Unsetenv("TZ"))
	assert.Equal(t, "Asia/Tokyo", LocalZone(path))
	t.Setenv("TZ", "Europe/Paris")
	assert.Equal(t, "Europe/Paris", LocalZone(path))
}
