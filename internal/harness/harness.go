// Package harness detects coding-agent identity using environment lookups and
// process ancestry without reading or changing the process environment.
package harness

import (
	"cmp"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/config"
)

// Detection carries the native session and unsanitized model identity.
type Detection struct {
	Harness   string
	SessionID string
	Model     string
	Provider  string
}

// Outcome distinguishes absent identity from conflicting harness markers.
type Outcome int

const (
	// None means no distinctive harness marker was present.
	None Outcome = iota
	// Detected means a canonical harness was selected.
	Detected
	// Ambiguous means marker families could not be resolved safely.
	Ambiguous
)

// Env looks up an environment key, preserving whether it was present.
type Env func(key string) (string, bool)

// Ancestry locates the nearest candidate harness and its process generation.
type Ancestry interface {
	Nearest(candidates []string) (harness, generation string, ok bool)
}

// StripEnv returns a copy of environ without harness marker or contract keys.
// Matching follows the platform's environment-key case sensitivity. Other
// entries retain their original order and contents.
func StripEnv(environ []string) []string {
	return config.StripHarnessEnv(environ)
}

func stripEnv(environ []string, caseInsensitive bool) []string {
	return config.FilterHarnessEnv(environ, caseInsensitive)
}

// Detect honors a canonical hook hint, otherwise resolving distinctive marker
// families through ancestry when nested. No distinctive marker yields None.
func Detect(env Env, ancestry Ancestry, hint string) (Detection, Outcome) {
	if selected := namedMarker(hint); selected != nil {
		return selected.detection(env), Detected
	}

	var candidates []string
	var selected *marker
	for i := range markers {
		candidate := &markers[i]
		if !candidate.present(env) {
			continue
		}
		candidates = append(candidates, candidate.name)
		selected = candidate
	}
	if selected == nil {
		return Detection{}, None
	}
	if len(candidates) > 1 {
		if ancestry == nil {
			return Detection{}, Ambiguous
		}
		// Ancestry is an interface; keep it from mutating the marker allowlist.
		nearest, _, ok := ancestry.Nearest(slices.Clone(candidates))
		if !ok || !slices.Contains(candidates, nearest) {
			return Detection{}, Ambiguous
		}
		selected = namedMarker(nearest)
		if selected == nil {
			return Detection{}, Ambiguous
		}
	}
	return selected.detection(env), Detected
}

func namedMarker(name string) *marker {
	for i := range markers {
		if markers[i].name == name {
			return &markers[i]
		}
	}
	return nil
}

func (m marker) detection(env Env) Detection {
	detection := Detection{Harness: m.name, SessionID: m.session(env)}
	if m.modelSource == modelNative {
		detection.Model = value(env, m.modelKey)
		detection.Provider = value(env, m.providerKey)
	}
	return bindModelContract(env, detection)
}

func value(env Env, key string) string {
	result, _ := env(key)
	return result
}

func (m marker) present(env Env) bool {
	for _, key := range m.markerKeys {
		if m.matches(value(env, key)) {
			return true
		}
	}
	return false
}

func (m marker) session(env Env) string {
	var session string
	for _, key := range m.sessionKeys {
		next := value(env, key)
		// Codex exports a thread ID and a session alias. Conflicting aliases
		// discard the session without discarding the harness identity.
		if session != "" && next != "" && !strings.EqualFold(session, next) {
			return ""
		}
		session = cmp.Or(session, next)
	}
	return session
}
