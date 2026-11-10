package metadata

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScheduleBundleDigest(t *testing.T) {
	require.Equal(t, ScheduleTZDataVersion, fmt.Sprintf("sha256:%x", sha256.Sum256(scheduleZoneBundle)))
	_, err := ScheduleLocation("Europe/Paris", ScheduleTZDataVersion)
	require.NoError(t, err)
}

func TestSavedScheduleIgnoresHostZoneinfo(t *testing.T) {
	if os.Getenv("KATA_SCHEDULE_ZONE_CHILD") == "1" {
		raw, err := NewScheduleValue("2026-11-12T09:00", "Europe/Paris")
		require.NoError(t, err)
		_, _, at, _, err := ScheduleFieldInstant(`{"scheduled_on":`+string(raw)+`}`, "scheduled_on", "UTC")
		require.NoError(t, err)
		require.Equal(t, "2026-11-12T08:00:00Z", at.Format(time.RFC3339))
		return
	}
	archive, err := zip.NewReader(bytes.NewReader(scheduleZoneBundle), int64(len(scheduleZoneBundle)))
	require.NoError(t, err)
	var utc []byte
	for _, f := range archive.File {
		if f.Name == "UTC" {
			r, err := f.Open()
			require.NoError(t, err)
			utc, err = io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())
			break
		}
	}
	require.NotEmpty(t, utc)
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	entry, err := z.Create("Europe/Paris")
	require.NoError(t, err)
	_, err = entry.Write(utc)
	require.NoError(t, err)
	require.NoError(t, z.Close())
	path := filepath.Join(t.TempDir(), "zoneinfo.zip")
	require.NoError(t, os.WriteFile(path, b.Bytes(), 0600))
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestSavedScheduleIgnoresHostZoneinfo$")
	cmd.Env = append(os.Environ(), "KATA_SCHEDULE_ZONE_CHILD=1", "ZONEINFO="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}
