package daemon

import (
	"errors"
	"strconv"
	"strings"
)

// WebRuntimeOptions is the browser metadata published alongside a daemon
// runtime record.
type WebRuntimeOptions struct {
	Origin       string
	OriginStable bool
	Capabilities []string
}

// WebRuntimeInfo is the validated browser metadata for one daemon process.
type WebRuntimeInfo struct {
	Origin       string
	OriginStable bool
	Capabilities []string
}

// NewWebRuntime validates browser metadata before it is advertised.
func NewWebRuntime(options WebRuntimeOptions) (WebRuntimeInfo, error) {
	if options.Origin == "" {
		return WebRuntimeInfo{}, errors.New("web runtime: origin is required")
	}
	if len(options.Capabilities) == 0 {
		return WebRuntimeInfo{}, errors.New("web runtime: capabilities are required")
	}
	for _, capability := range options.Capabilities {
		if capability == "" || strings.TrimSpace(capability) != capability {
			return WebRuntimeInfo{}, errors.New("web runtime: capabilities are invalid")
		}
	}
	return WebRuntimeInfo{
		Origin:       options.Origin,
		OriginStable: options.OriginStable,
		Capabilities: append([]string(nil), options.Capabilities...),
	}, nil
}

// Metadata returns the runtime-record keys used by browser launchers.
func (r WebRuntimeInfo) Metadata() map[string]string {
	return map[string]string{
		"web_origin":        r.Origin,
		"web_origin_stable": strconv.FormatBool(r.OriginStable),
		"web_capabilities":  strings.Join(r.Capabilities, ","),
	}
}
