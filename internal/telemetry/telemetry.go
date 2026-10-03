// Package telemetry emits anonymous, opt-out daemon, web UI, TUI and CLI usage events.
package telemetry

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kit/telemetry/posthog"
)

const (
	applicationName = "kata"
	envPrefix       = "KATA"
	// EnabledEnv controls anonymous telemetry; 0/false/no/off disable reporting.
	EnabledEnv = "KATA_TELEMETRY_ENABLED"
	// PostHog project API keys are public ingest identifiers, not credentials.
	postHogAPIKey   = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf" // #nosec G101
	postHogEndpoint = "https://us.i.posthog.com"
)

// ErrUnsupportedEvent is returned when callers try to capture an event outside the allowlist.
var ErrUnsupportedEvent = posthog.ErrUnsupportedEvent

// Client is the daemon-facing telemetry reporter contract. EventAllowed lets
// the UI capture route reject events outside the allowlist.
type Client interface {
	posthog.Client
	EventAllowed(event string) bool
}

// Reporter sanitizes and submits anonymous telemetry events to PostHog.
type Reporter = posthog.Reporter

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

// NewReporter builds an enabled reporter, or a disabled one that keeps the
// allowlist when telemetry is opted out or running under go test.
func NewReporter(opts Options) (*Reporter, error) {
	if testing.Testing() {
		// Go tests never send telemetry; kit's disabled reporter still admits allowed events.
		posthog.DisableProcess()
	}

	return posthog.NewReporter(posthog.Options{
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
		posthog.WithAllowedEvent("daemon_active",
			posthog.AllowProperty("project_count", posthog.AllowNumber),
		),
		posthog.WithAllowedEvent("daemon_started",
			posthog.AllowProperty("project_count", posthog.AllowNumber),
		),
		posthog.WithAllowedEvent("app_opened",
			posthog.AllowProperty("surface", appOpenedSurfaces),
		),
	)
}

// appOpenedSurfaces is the app_opened surface filter shared by the allowlist and the daemon's daily gate.
var appOpenedSurfaces = posthog.AllowStringValues("web", "tui", "cli")

// AppOpenedSurface returns the surface the reporter would keep for app_opened, or "" when it would keep none.
func AppOpenedSurface(properties map[string]any) string {
	for key, value := range properties {
		if strings.TrimSpace(key) != "surface" {
			continue
		}
		if kept, ok := appOpenedSurfaces(value); ok {
			surface, _ := kept.(string)
			return surface
		}
	}
	return ""
}

// DisabledReporter returns a reporter that drops events without network calls.
func DisabledReporter() *Reporter {
	return posthog.DisabledReporter()
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
