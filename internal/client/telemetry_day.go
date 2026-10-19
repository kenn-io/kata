package client

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/kata/internal/config"
)

// TelemetryDayMarkerPath retains the CLI and MCP marker identity for a resolved daemon.
func TelemetryDayMarkerPath(target ResolvedDaemon, prefix string) string {
	return TelemetryDayMarkerPathForKey(target.BaseURL+"\n"+target.UnixSocket, prefix)
}

// TelemetryTargetKey identifies one daemon across terminal interfaces.
func TelemetryTargetKey(target ResolvedDaemon) string {
	if profile := target.LocalProfile; profile != nil {
		return "profile:" + profile.DataDir + ":" + profile.InstanceUID
	}
	if target.Source == DaemonSourceLocalRuntime || (target.Source == DaemonSourceNamedCatalog && !target.namedRemote) {
		if target.RuntimeDir != "" {
			return "local:" + target.RuntimeDir
		}
		if dir, err := config.RuntimeDir(); err == nil {
			return "local:" + dir
		}
	}
	if target.UnixSocket != "" {
		return "unix:" + target.UnixSocket
	}
	return target.BaseURL
}

// TelemetryDisabledMarkerPath shares daily opt-out receipts across terminal callers.
func TelemetryDisabledMarkerPath(key string) string {
	return TelemetryDayMarkerPathForKey(key, "disabled-")
}

// TelemetryDayMarkerPathForKey hashes a stable target identity without storing its address.
func TelemetryDayMarkerPathForKey(key, prefix string) string {
	home, err := config.KataHome()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(home, "telemetry", prefix+hex.EncodeToString(sum[:8]))
}

// ReadTelemetryDayMarker returns the stored UTC date, or an empty string if unavailable.
func ReadTelemetryDayMarker(path string) string {
	if path == "" {
		return ""
	}
	day, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from KATA_HOME, not user input.
	if err != nil {
		return ""
	}
	return string(day)
}

// RecordTelemetryDayMarker is best effort: without the marker the next command reports again.
// It skips fsync because a lost marker costs only one more report, while a sync would delay
// the command's exit.
func RecordTelemetryDayMarker(path, day string) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = atomicfile.WriteFile(path, []byte(day), atomicfile.WithPerm(0o600), atomicfile.WithoutSync())
}
