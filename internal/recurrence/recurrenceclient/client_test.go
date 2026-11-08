package recurrenceclient

import (
	"os"
	"path/filepath"
	"testing"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLocalZone(t *testing.T) {
	link := func(t *testing.T, target string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "localtime")
		require.NoError(t, os.Symlink(target, path))
		return path
	}
	missing := filepath.Join(t.TempDir(), "absent")

	assert.Equal(t, "Europe/Paris", ResolveLocalZone("Europe/Paris", true, link(t, "/usr/share/zoneinfo/Asia/Tokyo")),
		"TZ wins over the localtime link")
	assert.Equal(t, "Europe/Paris", ResolveLocalZone(":Europe/Paris", true, missing), "POSIX leading colon")
	assert.Equal(t, "Asia/Tokyo", ResolveLocalZone("", false, link(t, "../usr/share/zoneinfo/Asia/Tokyo")))
	assert.Equal(t, "America/Argentina/Buenos_Aires",
		ResolveLocalZone("", false, link(t, "/var/db/timezone/zoneinfo/America/Argentina/Buenos_Aires")))
	assert.Equal(t, "Asia/Tokyo", ResolveLocalZone("CET-1", true, link(t, "/usr/share/zoneinfo/Asia/Tokyo")),
		"a TZ that is not an IANA name falls back to the link")
	assert.Equal(t, "Asia/Tokyo", ResolveLocalZone("Local", true, link(t, "/usr/share/zoneinfo/Asia/Tokyo")),
		"Local names no zone")
	assert.Equal(t, "UTC", ResolveLocalZone("", true, link(t, "/usr/share/zoneinfo/Asia/Tokyo")),
		"TZ set but empty means UTC")
	assert.Empty(t, ResolveLocalZone("", false, missing), "no zone known: the daemon decides")
	assert.Empty(t, ResolveLocalZone("", false, link(t, "/etc/some-other-file")))
}

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
