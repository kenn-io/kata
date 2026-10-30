package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLocalTimeZone(t *testing.T) {
	link := func(t *testing.T, target string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "localtime")
		require.NoError(t, os.Symlink(target, path))
		return path
	}
	missing := filepath.Join(t.TempDir(), "absent")

	assert.Equal(t, "Europe/Paris", resolveLocalTimeZone("Europe/Paris", true, link(t, "/usr/share/zoneinfo/Asia/Tokyo")),
		"TZ wins over the localtime link")
	assert.Equal(t, "Europe/Paris", resolveLocalTimeZone(":Europe/Paris", true, missing), "POSIX leading colon")
	assert.Equal(t, "Asia/Tokyo", resolveLocalTimeZone("", false, link(t, "../usr/share/zoneinfo/Asia/Tokyo")))
	assert.Equal(t, "America/Argentina/Buenos_Aires",
		resolveLocalTimeZone("", false, link(t, "/var/db/timezone/zoneinfo/America/Argentina/Buenos_Aires")))
	assert.Equal(t, "Asia/Tokyo", resolveLocalTimeZone("CET-1", true, link(t, "/usr/share/zoneinfo/Asia/Tokyo")),
		"a TZ that is not an IANA name falls back to the link")
	assert.Equal(t, "Asia/Tokyo", resolveLocalTimeZone("Local", true, link(t, "/usr/share/zoneinfo/Asia/Tokyo")),
		"Local names no zone")
	assert.Equal(t, "UTC", resolveLocalTimeZone("", true, link(t, "/usr/share/zoneinfo/Asia/Tokyo")),
		"TZ set but empty means UTC")
	assert.Empty(t, resolveLocalTimeZone("", false, missing), "no zone known: the daemon decides")
	assert.Empty(t, resolveLocalTimeZone("", false, link(t, "/etc/some-other-file")))
}

func TestLocalTimeZoneNameReadsLocaltimeLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "localtime")
	require.NoError(t, os.Symlink("/usr/share/zoneinfo/Asia/Tokyo", path))
	old := localtimePath
	localtimePath = path
	t.Cleanup(func() { localtimePath = old })

	t.Setenv("TZ", "")
	assert.Equal(t, "UTC", localTimeZoneName(), "TZ set but empty means UTC, not the localtime link")
	require.NoError(t, os.Unsetenv("TZ"))
	assert.Equal(t, "Asia/Tokyo", localTimeZoneName())
	t.Setenv("TZ", "Europe/Paris")
	assert.Equal(t, "Europe/Paris", localTimeZoneName())
}
