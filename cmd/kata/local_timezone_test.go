package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
