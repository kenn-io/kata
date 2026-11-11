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

func TestOpaqueRestoredDateDoesNotPretendToBeFutureSchedule(t *testing.T) {
	_, _, err := CalendarDate(`{"scheduled_on":{"note":"opaque"},"deadline_on":"2026-01-01"}`, "scheduled_on", nil, "UTC")
	require.Error(t, err)
	require.False(t, IsUnsupportedSchedule(err))
	_, _, err = CalendarDate(`{"scheduled_on":{"value":"2026-01-01","timezone":"UTC","tzdata_version":"future","new_field":true}}`, "scheduled_on", nil, "UTC")
	require.Error(t, err)
	require.True(t, IsUnsupportedSchedule(err))
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
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestSavedScheduleIgnoresHostZoneinfo$") // #nosec G204 -- re-execute this test binary with isolated timezone data.
	cmd.Env = append(os.Environ(), "KATA_SCHEDULE_ZONE_CHILD=1", "ZONEINFO="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestScheduleRetainedBundleRules(t *testing.T) {
	newer, err := os.ReadFile("testdata/zoneinfo-2026e-inuvik.zip")
	require.NoError(t, err)
	const newerVersion = "sha256:4e2962d1df08fd4a918e2c03cb999cee87360aa32d5aead1d4061b28c743b3dc"
	require.Equal(t, newerVersion, fmt.Sprintf("sha256:%x", sha256.Sum256(newer)))
	bundles := map[string][]byte{ScheduleTZDataVersion: scheduleZoneBundle, newerVersion: newer}
	old, err := scheduleLocationFromBundles(bundles, "America/Inuvik", ScheduleTZDataVersion)
	require.NoError(t, err)
	current, err := scheduleLocationFromBundles(bundles, "America/Inuvik", newerVersion)
	require.NoError(t, err)
	moment := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	_, oldOffset := moment.In(old).Zone()
	_, currentOffset := moment.In(current).Zone()
	require.Equal(t, -7*60*60, oldOffset)
	require.Equal(t, -6*60*60, currentOffset)
	again, err := scheduleLocationFromBundles(bundles, "America/Inuvik", ScheduleTZDataVersion)
	require.NoError(t, err)
	_, againOffset := moment.In(again).Zone()
	require.Equal(t, oldOffset, againOffset)
	_, err = scheduleLocationFromBundles(bundles, "America/Inuvik", "sha256:unknown")
	require.ErrorContains(t, err, "unsupported schedule tzdata version")
}
