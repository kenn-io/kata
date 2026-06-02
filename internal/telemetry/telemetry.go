package telemetry

import (
	"errors"
	"log/slog"
	"maps"
	"math"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/posthog/posthog-go"
)

const (
	EnabledEnv      = "KATA_TELEMETRY_ENABLED"
	postHogAPIKey   = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf"
	postHogEndpoint = "https://us.i.posthog.com"
)

var ErrUnsupportedEvent = errors.New("unsupported telemetry event")

type propertyFilter func(any) (any, bool)

var allowedEvents = map[string]map[string]propertyFilter{
	"daemon_active": {
		"project_count": safeTelemetryNumber,
	},
	"daemon_started": {
		"project_count": safeTelemetryNumber,
	},
}

type Client interface {
	Capture(event string, properties map[string]any) error
	Close() error
	Enabled() bool
}

type Reporter struct {
	client     enqueueCloser
	distinctID string
	enabled    bool
}

type enqueueCloser interface {
	Enqueue(posthog.Message) error
	Close() error
}

type Options struct {
	DistinctID string
	Version    string
	Commit     string
}

func EnabledFromEnv() bool {
	return strings.TrimSpace(os.Getenv(EnabledEnv)) != "0"
}

func EventAllowed(event string) bool {
	_, ok := allowedEvents[strings.TrimSpace(event)]
	return ok
}

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
	safeProperties["$geoip_disable"] = true
	return safeProperties, nil
}

func NewReporter(opts Options) (*Reporter, error) {
	if !EnabledFromEnv() {
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
			"app":            "kata",
			"source":         "daemon",
			"version":        opts.Version,
			"commit":         opts.Commit,
			"goos":           runtime.GOOS,
			"goarch":         runtime.GOARCH,
			"$geoip_disable": true,
		},
	})
	if err != nil {
		return nil, err
	}

	return &Reporter{
		client:     client,
		distinctID: distinctID,
		enabled:    true,
	}, nil
}

func DisabledReporter() *Reporter {
	return &Reporter{}
}

func NewReporterOrDisabled(opts Options) *Reporter {
	reporter, err := NewReporter(opts)
	if err != nil {
		slog.Warn("telemetry disabled", "err", err)
		return DisabledReporter()
	}
	return reporter
}

func (r *Reporter) Enabled() bool {
	return r != nil && r.enabled && r.client != nil
}

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

	return r.client.Enqueue(posthog.Capture{
		DistinctId: r.distinctID,
		Event:      event,
		Timestamp:  time.Now().UTC(),
		Properties: props,
	})
}

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
