package cron

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/mod/semver"
)

const (
	// RuntimeBeatLimit bounds the heartbeat request, independently of run evidence.
	RuntimeBeatLimit = 8 * 1024
	// RuntimeStartupGrace covers three missed beats at the largest allowed interval.
	RuntimeStartupGrace = 3 * time.Hour
)

// RuntimeBeat is self-reported evidence, never activation or execution authority.
type RuntimeBeat struct {
	Label           string   `json:"label"`
	Adapter         string   `json:"adapter"`
	AdapterVersion  string   `json:"adapter_version,omitempty"`
	KataVersion     string   `json:"kata_version,omitempty"`
	Features        []string `json:"features,omitempty"`
	IntervalSeconds int      `json:"interval_seconds,omitempty" default:"60" minimum:"10" maximum:"3600"`
	State           string   `json:"state,omitempty" default:"active" enum:"active,stopping"`
}

// Runtime adds server-owned attribution and observation times to a beat.
type Runtime struct {
	RuntimeBeat
	UID            string    `json:"runtime_uid"`
	Actor          string    `json:"actor"`
	FirstSeenAt    time.Time `json:"first_seen_at"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	Status         string    `json:"status" enum:"live,expired"`
	ReportedVia    string    `json:"reported_via" enum:"direct,forwarded"`
	ForwarderActor string    `json:"forwarder_actor,omitempty"`
}

// RuntimeJob is the minimal enabled-job input to advisory inventory warnings.
type RuntimeJob struct {
	UID        string
	Definition JobDefinition
}

// RuntimeWarning describes observed evidence rather than scheduler eligibility.
type RuntimeWarning struct {
	Code       string   `json:"code"`
	JobUID     string   `json:"job_uid,omitempty"`
	RuntimeUID string   `json:"runtime_uid,omitempty"`
	Adapter    string   `json:"adapter,omitempty"`
	Versions   []string `json:"versions,omitempty"`
	Message    string   `json:"message"`
}

// RuntimeInventory describes only the advertisements retained by this daemon.
type RuntimeInventory struct {
	Inventory RuntimeInventoryState `json:"inventory"`
	Runtimes  []Runtime             `json:"runtimes"`
	Warnings  []RuntimeWarning      `json:"warnings"`
}

// RuntimeInventoryState is unknown without retained history, including after eviction.
type RuntimeInventoryState struct {
	State           string    `json:"state" enum:"unknown,warming,ready"`
	DaemonStartedAt time.Time `json:"daemon_started_at"`
	WarmingUntil    time.Time `json:"warming_until"`
}

var runtimeToken = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

// NormalizeRuntimeBeat validates the shared direct/forwarded input and owns its slices.
// Zero interval and empty state are internal defaults; HTTP rejects explicit zero/null.
func NormalizeRuntimeBeat(b RuntimeBeat) (RuntimeBeat, error) {
	invalid := func(field string) (RuntimeBeat, error) {
		return RuntimeBeat{}, fmt.Errorf("%w: invalid runtime %s", ErrInvalid, field)
	}
	if len(b.Label) > 256 || strings.TrimSpace(b.Label) == "" || !utf8.ValidString(b.Label) {
		return invalid("label")
	}
	if !runtimeToken.MatchString(b.Adapter) {
		return invalid("adapter")
	}
	for _, v := range []string{b.AdapterVersion, b.KataVersion} {
		if len(v) > 64 {
			return invalid("version")
		}
		for _, c := range []byte(v) {
			if c < 32 || c > 126 {
				return invalid("version")
			}
		}
	}
	if len(b.Features) > 32 {
		return invalid("features")
	}
	b.Features = slices.Clone(b.Features)
	if b.Features == nil {
		b.Features = []string{}
	}
	slices.Sort(b.Features)
	for i, f := range b.Features {
		if !runtimeToken.MatchString(f) || i > 0 && b.Features[i-1] == f {
			return invalid("features")
		}
	}
	if b.IntervalSeconds == 0 {
		b.IntervalSeconds = 60
	}
	if b.IntervalSeconds < 10 || b.IntervalSeconds > 3600 {
		return invalid("interval_seconds")
	}
	if b.State == "" {
		b.State = "active"
	}
	if b.State != "active" && b.State != "stopping" {
		return invalid("state")
	}
	return b, nil
}

// RuntimeStatus uses a half-open liveness interval; a stopping beat expires immediately.
func RuntimeStatus(r Runtime, now time.Time) string {
	if r.State == "stopping" || !now.Before(r.ExpiresAt) {
		return "expired"
	}
	return "live"
}

// EvaluateRuntimes returns deterministic independent copies and advisory warnings.
// The caller supplies only undeleted jobs. Advertisements never prove eligibility.
func EvaluateRuntimes(entries []Runtime, jobs []RuntimeJob, now time.Time, daemonVersion string, startedAt time.Time) RuntimeInventory {
	out := RuntimeInventory{Inventory: RuntimeInventoryState{State: "unknown", DaemonStartedAt: startedAt, WarmingUntil: startedAt.Add(RuntimeStartupGrace)}, Runtimes: []Runtime{}, Warnings: []RuntimeWarning{}}
	if len(entries) > 0 {
		out.Inventory.State = "ready"
		if now.Before(out.Inventory.WarmingUntil) {
			out.Inventory.State = "warming"
		}
	}
	live := []Runtime{}
	for _, r := range entries {
		r.Features = slices.Clone(r.Features)
		r.Status = RuntimeStatus(r, now)
		out.Runtimes = append(out.Runtimes, r)
		if r.Status == "live" {
			live = append(live, r)
		}
	}
	slices.SortFunc(out.Runtimes, func(a, b Runtime) int {
		if n := cmp.Compare(a.Label, b.Label); n != 0 {
			return n
		}
		return cmp.Compare(a.UID, b.UID)
	})
	versions := map[string][]string{}
	for _, r := range live {
		if r.AdapterVersion != "" {
			versions[r.Adapter] = append(versions[r.Adapter], r.AdapterVersion)
		}
		if dm, rm := runtimeMajorMinor(daemonVersion), runtimeMajorMinor(r.KataVersion); dm != "" && rm != "" && dm != rm {
			out.Warnings = append(out.Warnings, RuntimeWarning{Code: "kata_version_skew", RuntimeUID: r.UID, Message: fmt.Sprintf("runtime reports Kata %s; daemon reports %s", r.KataVersion, daemonVersion)})
		}
	}
	for adapter, values := range versions {
		slices.Sort(values)
		values = slices.Compact(values)
		if len(values) > 1 {
			out.Warnings = append(out.Warnings, RuntimeWarning{Code: "adapter_version_skew", Adapter: adapter, Versions: values, Message: fmt.Sprintf("adapter %s advertises versions %s", adapter, strings.Join(values, ", "))})
		}
	}
	if out.Inventory.State == "ready" {
		execute := false
		for _, job := range jobs {
			if !job.Definition.Enabled || job.Definition.Action.Kind != "execute" {
				continue
			}
			execute = true
			pin, adapter := runtimeSelectors(job.Definition.Options)
			matched := false
			for _, r := range live {
				if (pin == "" || r.Label == pin) && (adapter == "" || r.Adapter == adapter) {
					matched = true
					break
				}
			}
			if pin != "" && !matched {
				out.Warnings = append(out.Warnings, RuntimeWarning{Code: "pin_unmatched", JobUID: job.UID, Message: fmt.Sprintf("no matching live advertisement for executor %q in this daemon's inventory", pin)})
			} else if pin == "" && adapter != "" && !matched {
				out.Warnings = append(out.Warnings, RuntimeWarning{Code: "adapter_unmatched", JobUID: job.UID, Adapter: adapter, Message: fmt.Sprintf("no matching live advertisement for adapter %q in this daemon's inventory", adapter)})
			}
		}
		if execute && len(live) == 0 {
			out.Warnings = append(out.Warnings, RuntimeWarning{Code: "no_live_runtime", Message: "no live runtime advertisement in this daemon's inventory"})
		}
	}
	slices.SortFunc(out.Warnings, func(a, b RuntimeWarning) int {
		for _, pair := range [][2]string{{a.Code, b.Code}, {a.JobUID, b.JobUID}, {a.RuntimeUID, b.RuntimeUID}, {a.Adapter, b.Adapter}, {a.Message, b.Message}} {
			if n := cmp.Compare(pair[0], pair[1]); n != 0 {
				return n
			}
		}
		return 0
	})
	return out
}

func runtimeSelectors(raw jsontext.Value) (string, string) {
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil {
		return "", ""
	}
	read := func(key string) string {
		var s string
		if json.Unmarshal(fields[key], &s) != nil || strings.TrimSpace(s) == "" {
			return ""
		}
		return s
	}
	return read("executor"), read("adapter")
}
func runtimeMajorMinor(v string) string {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) || semver.Prerelease(v) != "" || strings.Count(strings.TrimSuffix(v, semver.Build(v)), ".") != 2 {
		return ""
	}
	return semver.MajorMinor(v)
}
