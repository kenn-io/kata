// Package telemetry emits anonymous, opt-out daemon usage events.
package telemetry

import (
	"errors"
	"log/slog"
	"maps"
	"math"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/posthog/posthog-go"
)

const (
	applicationName = "kata"
	// EnabledEnv controls anonymous telemetry; set it to "0" to disable reporting.
	EnabledEnv = "KATA_TELEMETRY_ENABLED"
	// PostHog project API keys are public ingest identifiers, not credentials.
	postHogAPIKey   = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf" // #nosec G101
	postHogEndpoint = "https://us.i.posthog.com"
)

// ErrUnsupportedEvent is returned when callers try to capture an event outside the allowlist.
var ErrUnsupportedEvent = errors.New("unsupported telemetry event")

var postHogTelemetryDisabledState atomic.Bool

func init() {
	if testing.Testing() {
		disablePostHogTelemetry()
	}
}

func postHogTelemetryDisabled() bool {
	return postHogTelemetryDisabledState.Load()
}

func disablePostHogTelemetry() {
	postHogTelemetryDisabledState.Store(true)
}

type propertyFilter func(any) (any, bool)

var allowedEvents = map[string]map[string]propertyFilter{
	"daemon_active": {
		"project_count": safeTelemetryNumber,
	},
	"daemon_started": {
		"project_count": safeTelemetryNumber,
	},
}

// Client is the daemon-facing telemetry reporter contract.
type Client interface {
	Capture(event string, properties map[string]any) error
	Close() error
	Enabled() bool
}

// Reporter sanitizes and submits anonymous telemetry events to PostHog.
type Reporter struct {
	client     enqueueCloser
	distinctID string
	version    string
	commit     string
	enabled    bool
}

type enqueueCloser interface {
	Enqueue(posthog.Message) error
	Close() error
}

// Options configures a telemetry reporter instance.
type Options struct {
	DistinctID string
	Version    string
	Commit     string
}

// EnabledFromEnv reports whether anonymous telemetry is enabled by the environment.
func EnabledFromEnv() bool {
	if postHogTelemetryDisabled() {
		return false
	}
	return strings.TrimSpace(os.Getenv(EnabledEnv)) != "0"
}

// EventAllowed reports whether event is included in the telemetry allowlist.
func EventAllowed(event string) bool {
	_, ok := allowedEvents[strings.TrimSpace(event)]
	return ok
}

// SanitizeProperties returns only allowlisted properties for event.
func SanitizeProperties(event string, properties map[string]any) (map[string]any, error) {
	allowedProperties, ok := allowedEvents[strings.TrimSpace(event)]
	if !ok {
		return nil, ErrUnsupportedEvent
	}

	safeProperties := map[string]any{}
	for key, value := range properties {
		key = strings.TrimSpace(key)
		filter, ok := allowedProperties[key]
		if !ok {
			continue
		}
		if safeValue, ok := filter(value); ok {
			safeProperties[key] = safeValue
		}
	}
	safeProperties["$process_person_profile"] = false
	safeProperties["$geoip_disable"] = true
	safeProperties["application"] = applicationName
	return safeProperties, nil
}

// NewReporter builds an enabled reporter or returns a disabled reporter when opted out.
func NewReporter(opts Options) (*Reporter, error) {
	return newReporter(opts, EnabledFromEnv())
}

func newReporter(opts Options, enabled bool) (*Reporter, error) {
	if !enabled {
		return DisabledReporter(), nil
	}
	distinctID := strings.TrimSpace(opts.DistinctID)
	if distinctID == "" {
		return nil, errors.New("telemetry distinct id is required")
	}

	disableGeoIP := true
	client, err := posthog.NewWithConfig(postHogAPIKey, posthog.Config{
		Endpoint:     postHogEndpoint,
		DisableGeoIP: &disableGeoIP,
		DefaultEventProperties: posthog.Properties{
			"application":             applicationName,
			"source":                  "daemon",
			"version":                 opts.Version,
			"commit":                  opts.Commit,
			"goos":                    runtime.GOOS,
			"goarch":                  runtime.GOARCH,
			"$process_person_profile": false,
			"$geoip_disable":          true,
		},
	})
	if err != nil {
		return nil, err
	}

	return &Reporter{
		client:     client,
		distinctID: distinctID,
		version:    opts.Version,
		commit:     opts.Commit,
		enabled:    true,
	}, nil
}

// DisabledReporter returns a reporter that drops events without network calls.
func DisabledReporter() *Reporter {
	return &Reporter{}
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

// Enabled reports whether the reporter can submit telemetry events.
func (r *Reporter) Enabled() bool {
	return r != nil && r.enabled && r.client != nil
}

// Capture sanitizes and queues an anonymous telemetry event.
func (r *Reporter) Capture(event string, properties map[string]any) error {
	if !r.Enabled() {
		return nil
	}

	event = strings.TrimSpace(event)
	if event == "" {
		return errors.New("telemetry event is required")
	}

	safeProperties, err := SanitizeProperties(event, properties)
	if err != nil {
		return err
	}

	props := posthog.Properties{}
	maps.Copy(props, safeProperties)
	props["$process_person_profile"] = false
	props["$geoip_disable"] = true
	props["application"] = applicationName
	props["version"] = r.version
	props["commit"] = r.commit
	props["goos"] = runtime.GOOS
	props["goarch"] = runtime.GOARCH
	props["source"] = "daemon"

	return r.client.Enqueue(posthog.Capture{
		DistinctId: r.distinctID,
		Event:      event,
		Timestamp:  time.Now().UTC(),
		Properties: props,
	})
}

// Close flushes pending telemetry events when the reporter is enabled.
func (r *Reporter) Close() error {
	if !r.Enabled() {
		return nil
	}
	return r.client.Close()
}

func safeTelemetryNumber(value any) (any, bool) {
	switch v := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, true
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, false
		}
		return v, true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		return v, true
	default:
		return nil, false
	}
}
