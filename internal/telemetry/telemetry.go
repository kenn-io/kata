// Package telemetry emits anonymous, opt-out daemon, web UI, TUI and CLI usage events.
package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kit/telemetry/posthog"
)

const (
	// ExitWait caps how long a completed command or TUI session waits for telemetry.
	ExitWait = 50 * time.Millisecond

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

// ErrInvalidProperty is returned when an event lacks a required property or its filter rejects it.
var ErrInvalidProperty = posthog.ErrInvalidProperty

// Client is the daemon-facing telemetry reporter contract. EventAllowed lets
// the UI capture route reject events outside the allowlist.
type Client interface {
	posthog.Client
	CaptureConfirmed(context.Context, string, map[string]any, posthog.Delivery) (posthog.Status, error)
	EventAllowed(event string) bool
	SanitizeProperties(event string, properties map[string]any) (map[string]any, error)
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
		posthog.WithAllowedEvent("setup_finished"),
		posthog.WithAllowedEvent("daemon_active",
			posthog.AllowProperty("project_count", projectCountBuckets),
		),
		posthog.WithAllowedEvent("daemon_started",
			posthog.AllowProperty("project_count", projectCountBuckets),
		),
		posthog.WithAllowedEvent("app_opened",
			posthog.AllowProperty("surface", validSurfaces),
		),
		posthog.WithAllowedEvent("issue_created",
			posthog.RequireProperty("surface", validSurfaces),
		),
		posthog.WithAllowedEvent("issue_closed",
			posthog.RequireProperty("surface", validSurfaces),
		),
		posthog.WithAllowedEvent("search_empty", posthog.RequireProperty("surface", validSurfaces)),
		posthog.WithAllowedEvent("error_shown",
			posthog.RequireProperty("surface", validSurfaces),
			posthog.RequireProperty("kind", outcomeValues("issue_created", "issue_closed")),
		),
		posthog.WithAllowedEvent("sync_failed",
			posthog.RequireProperty("sync_source", outcomeValues("github", "linear", "notion", "plane", "ticktick", "todoist", "twenty", "federation")),
			posthog.RequireProperty("reason", outcomeValues("auth", "rate_limit", "server", "network", "timeout")),
		),
		posthog.WithAllowedEvent("session_ended",
			posthog.AllowProperty("surface", posthog.AllowStringValues("web", "tui")),
			posthog.AllowProperty("duration_bucket", posthog.AllowStringValues("under_1m", "1_to_5m", "5_to_30m", "over_30m")),
		),
		posthog.WithAllowedEvent("agent_active",
			posthog.AllowProperty("call_count_bucket", agentCallCountBuckets),
		),
		posthog.WithAllowedEvent("agent_call_count",
			posthog.AllowProperty("call_count_bucket", agentCallCountBuckets),
		),
		posthog.WithAllowedEvent("screen_viewed",
			posthog.AllowProperty("screen", screenNames),
			posthog.AllowProperty("surface", screenSurfaces),
		),
	)
}

// DurationBucket groups elapsed time without sending an exact duration.
func DurationBucket(elapsed time.Duration) string {
	switch {
	case elapsed < time.Minute:
		return "under_1m"
	case elapsed < 5*time.Minute:
		return "1_to_5m"
	case elapsed <= 30*time.Minute:
		return "5_to_30m"
	default:
		return "over_30m"
	}
}

const (
	projectCount0To5    = "0-5"
	projectCount6To20   = "6-20"
	projectCount21To100 = "21-100"
	projectCountOver100 = "over-100"
)

// ProjectCountBucket groups project totals without sending an exact count.
func ProjectCountBucket(count int) string {
	switch {
	case count <= 5:
		return projectCount0To5
	case count <= 20:
		return projectCount6To20
	case count <= 100:
		return projectCount21To100
	default:
		return projectCountOver100
	}
}

var projectCountBuckets = posthog.AllowStringValues(projectCount0To5, projectCount6To20, projectCount21To100, projectCountOver100)

var agentCallCountBuckets = posthog.AllowStringValues("1-10", "11-100", "over-100")

// validSurfaces filters interface names shared by surface-attributed events.
var validSurfaces = posthog.AllowStringValues("web", "tui", "cli")

// AppOpenedSurface returns the surface the reporter would keep for app_opened, or "" when it would keep none.
func AppOpenedSurface(properties map[string]any) string {
	for key, value := range properties {
		if strings.TrimSpace(key) != "surface" {
			continue
		}
		if kept, ok := validSurfaces(value); ok {
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

var screenNames = posthog.AllowStringValues("inbox", "today", "delegated", "scheduled", "issues", "logbook", "issue", "graph", "credentials", "projects", "daemons", "federation", "help", "empty")
var screenSurfaces = posthog.AllowStringValues("web", "tui")

// outcomeValues collapses unlisted values without sending free text.
func outcomeValues(values ...string) func(any) (any, bool) {
	allowed := posthog.AllowStringValues(values...)
	return func(value any) (any, bool) {
		if kept, ok := allowed(value); ok {
			return kept, true
		}
		return "other", true
	}
}
