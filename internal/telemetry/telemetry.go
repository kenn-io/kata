// Package telemetry emits anonymous, opt-out daemon and web UI usage events.
package telemetry

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	kittelemetry "go.kenn.io/kit/telemetry"
)

const (
	applicationName = "kata"
	envPrefix       = "KATA"
	// EnabledEnv controls anonymous telemetry; set it to "0" to disable reporting.
	EnabledEnv = "KATA_TELEMETRY_ENABLED"
	// PostHog project API keys are public ingest identifiers, not credentials.
	postHogAPIKey   = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf" // #nosec G101
	postHogEndpoint = "https://us.i.posthog.com"
)

// ErrUnsupportedEvent is returned when callers try to capture an event outside the allowlist.
var ErrUnsupportedEvent = kittelemetry.ErrUnsupportedTelemetryEvent

// Client is the daemon-facing telemetry reporter contract.
type Client = kittelemetry.PostHogClient

// Reporter sanitizes and submits anonymous telemetry events to PostHog.
type Reporter = kittelemetry.PostHogReporter

// Options configures a telemetry reporter instance.
type Options struct {
	DistinctID string
	// InstalledAt is when DistinctID was created; events carry its age as
	// install_age_hours. Zero, for an install that predates recording it,
	// sends events without an age.
	InstalledAt time.Time
	Version     string
	Commit      string
}

// EnabledFromEnv reports whether anonymous telemetry is enabled by the environment.
func EnabledFromEnv() bool {
	return !testing.Testing() && kittelemetry.PostHogTelemetryEnabledFromEnv(envPrefix)
}

// NewReporter builds an enabled reporter, or a disabled one that keeps the
// allowlist when telemetry is opted out or running under go test.
func NewReporter(opts Options) (*Reporter, error) {
	if testing.Testing() {
		// Go tests never send telemetry; kit's disabled reporter still admits allowed events.
		kittelemetry.DisablePostHogTelemetry()
	}

	return kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      postHogAPIKey,
		Endpoint:    postHogEndpoint,
		Application: applicationName,
		EnvPrefix:   envPrefix,
		DistinctID:  opts.DistinctID,
		InstalledAt: opts.InstalledAt,
		Version:     opts.Version,
		Commit:      opts.Commit,
		Source:      "daemon",
	},
		kittelemetry.WithAllowedEvent("daemon_active",
			kittelemetry.AllowTelemetryProperty("project_count", kittelemetry.AllowTelemetryNumber),
		),
		kittelemetry.WithAllowedEvent("daemon_started",
			kittelemetry.AllowTelemetryProperty("project_count", kittelemetry.AllowTelemetryNumber),
		),
		kittelemetry.WithAllowedEvent("app_opened"),
	)
}

// DisabledReporter returns a reporter that drops events without network calls.
func DisabledReporter() *Reporter {
	return kittelemetry.DisabledPostHogReporter()
}

// CaptureHandler lets the web UI report allowlisted events through client.
// A client that is not a *Reporter admits no event.
func CaptureHandler(client Client) http.Handler {
	reporter, _ := client.(*Reporter)
	return kittelemetry.NewPostHogCaptureHandler(reporter)
}

// NewReporterOrDisabled builds a reporter and falls back to a disabled reporter on errors.
func NewReporterOrDisabled(opts Options) *Reporter {
	reporter, err := NewReporter(opts)
	if err != nil {
		slog.Warn("telemetry disabled", "err", err)
		return DisabledReporter()
	}
	return reporter
}
