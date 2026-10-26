package harness

import (
	"slices"
	"testing"
)

const sessionID = "019a1359-6b50-7a55-bdef-0123456789ab"

type ancestryFunc func([]string) (string, string, bool)

func (f ancestryFunc) Nearest(candidates []string) (string, string, bool) {
	return f(candidates)
}

func lookup(values map[string]string) Env {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// Each fixture pins a distinctive marker and its own session/model sources.
func TestDetectSingleMarker(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Detection
	}{
		{"claude", map[string]string{"CLAUDE_CODE_SESSION_ID": sessionID}, Detection{Harness: "claude", SessionID: sessionID}},
		{"claude uppercase UUID", map[string]string{"CLAUDE_CODE_SESSION_ID": "019A1359-6B50-7A55-BDEF-0123456789AB"}, Detection{Harness: "claude", SessionID: "019A1359-6B50-7A55-BDEF-0123456789AB"}},
		{"codex thread", map[string]string{"CODEX_THREAD_ID": sessionID}, Detection{Harness: "codex", SessionID: sessionID}},
		{"codex alias", map[string]string{"CODEX_SESSION_ID": sessionID}, Detection{Harness: "codex", SessionID: sessionID}},
		{"pi", map[string]string{"PI_CODING_AGENT": "true", "PI_SESSION_ID": sessionID, "PI_PROVIDER": "zai", "PI_MODEL": "glm-5.3"}, Detection{Harness: "pi", SessionID: sessionID, Provider: "zai", Model: "glm-5.3"}},
		{"omp", map[string]string{"OMPCODE": "1", "PI_SESSION_ID": sessionID}, Detection{Harness: "omp", SessionID: sessionID}},
		{"amp", map[string]string{"AGENT": "amp"}, Detection{Harness: "amp"}},
		{"openclaw", map[string]string{"OPENCLAW_SHELL": "exec"}, Detection{Harness: "openclaw"}},
		{"cursor", map[string]string{"CURSOR_AGENT": "1"}, Detection{Harness: "cursor"}},
		{"gemini", map[string]string{"GEMINI_CLI": "1"}, Detection{Harness: "gemini"}},
		{"qwen", map[string]string{"QWEN_CODE": "1"}, Detection{Harness: "qwen"}},
		{"pi without session", map[string]string{"PI_CODING_AGENT": "true"}, Detection{Harness: "pi"}},
		{"omp ignores native pi model", map[string]string{"OMPCODE": "1", "PI_PROVIDER": "zai", "PI_MODEL": "glm-5.3"}, Detection{Harness: "omp"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, outcome := Detect(lookup(tt.env), nil, "")
			if outcome != Detected || got != tt.want {
				t.Fatalf("Detect() = (%+v, %v), want (%+v, Detected)", got, outcome, tt.want)
			}
		})
	}
}

func TestDetectIgnoresNonDistinctive(t *testing.T) {
	got, outcome := Detect(lookup(map[string]string{"CLAUDECODE": "1", "CI": "true", "AI_AGENT": "x"}), nil, "")
	if outcome != None || got != (Detection{}) {
		t.Fatalf("Detect() = (%+v, %v), want empty None", got, outcome)
	}
}

// Reject false positives: marker presence alone is insufficient except for Codex.
func TestDetectRejectsInvalidMarkers(t *testing.T) {
	tests := []struct{ key, value string }{
		{"CLAUDE_CODE_SESSION_ID", "not-a-uuid"},
		{"CLAUDE_CODE_SESSION_ID", "019a1359-6b50-7a55-bdef-0123456789ag"},
		{"CLAUDE_CODE_SESSION_ID", "019a13596b507a55bdef0123456789ab"},
		{"CLAUDE_CODE_SESSION_ID", ""},
		{"CODEX_THREAD_ID", ""}, {"CODEX_SESSION_ID", ""},
		{"PI_CODING_AGENT", "1"}, {"PI_CODING_AGENT", "TRUE"},
		{"OMPCODE", "true"}, {"AGENT", "other"},
		{"OPENCLAW_SHELL", "1"}, {"CURSOR_AGENT", "true"},
		{"GEMINI_CLI", "true"}, {"QWEN_CODE", "true"},
		{"PI_SESSION_ID", sessionID}, {"PI_MODEL", "glm-5.3"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			got, outcome := Detect(lookup(map[string]string{tt.key: tt.value}), nil, "")
			if outcome != None || got != (Detection{}) {
				t.Fatalf("Detect() = (%+v, %v), want empty None", got, outcome)
			}
		})
	}
}

func TestDetectCodexAlias(t *testing.T) {
	tests := []struct{ name, thread, alias, want string }{
		{"equal ignoring case", "019A1359-6B50-7A55-BDEF-0123456789AB", sessionID, "019A1359-6B50-7A55-BDEF-0123456789AB"},
		{"different", sessionID, "019a1359-6b50-7a55-bdef-0123456789ac", ""},
		{"thread only", sessionID, "", sessionID},
		{"alias only", "", sessionID, sessionID},
		{"nonempty native value", "native-thread", "", "native-thread"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, outcome := Detect(lookup(map[string]string{"CODEX_THREAD_ID": tt.thread, "CODEX_SESSION_ID": tt.alias}), nil, "")
			want := Detection{Harness: "codex", SessionID: tt.want}
			if outcome != Detected || got != want {
				t.Fatalf("Detect() = (%+v, %v), want (%+v, Detected)", got, outcome, want)
			}
		})
	}
}

func TestDetectMultipleFamilies(t *testing.T) {
	for _, env := range []map[string]string{
		{"CLAUDE_CODE_SESSION_ID": sessionID, "CODEX_THREAD_ID": sessionID},
		{"PI_CODING_AGENT": "true", "OMPCODE": "1", "PI_SESSION_ID": sessionID},
	} {
		got, outcome := Detect(lookup(env), nil, "")
		if outcome != Ambiguous || got != (Detection{}) {
			t.Fatalf("Detect() = (%+v, %v), want empty Ambiguous", got, outcome)
		}
	}
}

// EnvKeys drives daemon scrubbing, so omissions can leak a stale identity.
func TestEnvKeys(t *testing.T) {
	want := []string{"AGENT", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID", "CURSOR_AGENT", "GEMINI_CLI", "KATA_HARNESS_MODEL", "KATA_HARNESS_SESSION", "OMPCODE", "OPENCLAW_SHELL", "PI_CODING_AGENT", "PI_MODEL", "PI_PROVIDER", "PI_SESSION_ID", "QWEN_CODE"}
	if got := EnvKeys(); !slices.Equal(got, want) {
		t.Fatalf("EnvKeys() = %q, want %q", got, want)
	}
	keys := EnvKeys()
	keys[0] = "corrupted"
	if got := EnvKeys(); !slices.Equal(got, want) {
		t.Fatalf("mutating returned keys changed registry: %q", got)
	}
}

func TestNames(t *testing.T) {
	want := []string{"amp", "claude", "codex", "cursor", "gemini", "muse", "omp", "openclaw", "pi", "qwen"}
	got := Names()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("Names() = %q, want %q", got, want)
	}
	got[0] = "corrupted"
	names := Names()
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("mutating returned names changed registry: %q", names)
	}
}

func TestDetectNestedSessions(t *testing.T) {
	for _, tt := range []struct {
		name   string
		env    map[string]string
		winner string
		want   Detection
	}{
		{"OMP with Pi", map[string]string{"OMPCODE": "1", "PI_CODING_AGENT": "true", "PI_SESSION_ID": "pi-session"}, "pi", Detection{Harness: "pi", SessionID: "pi-session"}},
		{"Claude with Codex", map[string]string{"CLAUDE_CODE_SESSION_ID": sessionID, "CODEX_THREAD_ID": "codex-thread"}, "codex", Detection{Harness: "codex", SessionID: "codex-thread"}},
		{"Codex conflicting aliases", map[string]string{"CLAUDE_CODE_SESSION_ID": sessionID, "CODEX_THREAD_ID": "thread", "CODEX_SESSION_ID": "other"}, "codex", Detection{Harness: "codex"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := ancestryFunc(func([]string) (string, string, bool) { return tt.winner, "42:123456", true })
			got, outcome := Detect(lookup(tt.env), probe, "")
			if outcome != Detected || got != tt.want {
				t.Fatalf("Detect() = (%+v, %v), want (%+v, Detected)", got, outcome, tt.want)
			}
		})
	}
}

func TestDetectNestedPiPackageEntrypoints(t *testing.T) {
	for _, entrypoint := range []string{
		"/x/node_modules/@earendil-works/pi-coding-agent/dist/cli.js",
		"/x/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js",
		`C:\tools\node_modules\@earendil-works\pi-coding-agent\dist\bundle\cli.js`,
	} {
		t.Run(entrypoint, func(t *testing.T) {
			probe := fixtureAncestry(processFixture{
				name:    "node",
				args:    []string{"node", entrypoint},
				created: 123456,
			})
			env := lookup(map[string]string{
				"PI_CODING_AGENT": "true",
				"PI_SESSION_ID":   "pi-session",
				"CODEX_THREAD_ID": "codex-thread",
			})
			got, outcome := Detect(env, probe, "")
			want := Detection{Harness: "pi", SessionID: "pi-session"}
			if outcome != Detected || got != want {
				t.Fatalf("Detect() = (%+v, %v), want (%+v, Detected)", got, outcome, want)
			}
		})
	}
}

func TestDetectProbeErrorIsAmbiguousNotFatal(t *testing.T) {
	for _, tt := range []struct {
		name, result string
		ok           bool
	}{
		{"probe error", "codex", false},
		{"no match", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := ancestryFunc(func([]string) (string, string, bool) { return tt.result, "42:123456", tt.ok })
			got, outcome := Detect(lookup(map[string]string{"CLAUDE_CODE_SESSION_ID": sessionID, "CODEX_THREAD_ID": "thread"}), probe, "")
			if outcome != Ambiguous || got != (Detection{}) {
				t.Fatalf("Detect() = (%+v, %v), want empty Ambiguous", got, outcome)
			}
		})
	}
}
func TestDetectHintWins(t *testing.T) {
	probe := ancestryFunc(func([]string) (string, string, bool) {
		t.Fatal("a canonical hint must resolve before probing ancestry")
		return "", "", false
	})
	for _, hint := range Names() {
		got, outcome := Detect(lookup(map[string]string{"CODEX_THREAD_ID": "thread", "CLAUDE_CODE_SESSION_ID": sessionID}), probe, hint)
		want := Detection{Harness: hint}
		switch hint {
		case "codex":
			want.SessionID = "thread"
		case "claude":
			want.SessionID = sessionID
		}
		if outcome != Detected || got != want {
			t.Fatalf("Detect(hint=%q) = (%+v, %v), want (%+v, Detected)", hint, got, outcome, want)
		}
	}
	got, outcome := Detect(lookup(nil), nil, "muse")
	if outcome != Detected || got != (Detection{Harness: "muse"}) {
		t.Fatalf("Muse hook hint = (%+v, %v), want muse Detected", got, outcome)
	}
}

func TestDetectUnknownHintIgnored(t *testing.T) {
	for _, hint := range []string{"unknown", "CODEX", "pi/other", " "} {
		got, outcome := Detect(lookup(map[string]string{"CODEX_THREAD_ID": "thread"}), nil, hint)
		if outcome != Detected || got != (Detection{Harness: "codex", SessionID: "thread"}) {
			t.Fatalf("unknown hint %q changed marker detection: (%+v, %v)", hint, got, outcome)
		}
	}
}

func TestDetectZeroMarkerAncestryRemainsDeferred(t *testing.T) {
	probe := ancestryFunc(func([]string) (string, string, bool) {
		t.Fatal("zero-marker ancestry belongs to the later Muse integration")
		return "muse", "42:123456", true
	})
	got, outcome := Detect(lookup(nil), probe, "")
	if outcome != None || got != (Detection{}) {
		t.Fatalf("Detect() = (%+v, %v), want empty None", got, outcome)
	}
}

func TestDetectContractParsing(t *testing.T) {
	for _, tt := range []struct {
		name, binding, model string
		want                 Detection
	}{
		{"provider and model", "omp:session", "provider/model/variant", Detection{Harness: "omp", SessionID: "session", Provider: "provider", Model: "model/variant"}},
		{"model only", "omp:session", "model", Detection{Harness: "omp", SessionID: "session", Model: "model"}},
		{"wrong session", "omp:other", "provider/model", Detection{Harness: "omp", SessionID: "session"}},
		{"wrong harness", "claude:session", "provider/model", Detection{Harness: "omp", SessionID: "session"}},
		{"case differs", "OMP:session", "provider/model", Detection{Harness: "omp", SessionID: "session"}},
		{"empty model", "omp:session", "", Detection{Harness: "omp", SessionID: "session"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"OMPCODE": "1", "PI_SESSION_ID": "session", "KATA_HARNESS_SESSION": tt.binding, "KATA_HARNESS_MODEL": tt.model}
			got, outcome := Detect(lookup(env), nil, "")
			if outcome != Detected || got != tt.want {
				t.Fatalf("Detect() = (%+v, %v), want (%+v, Detected)", got, outcome, tt.want)
			}
		})
	}
}

func TestDetectNativePiModelWins(t *testing.T) {
	for _, tt := range []struct {
		model, provider string
		want            Detection
	}{
		{"native", "native-provider", Detection{Harness: "pi", SessionID: "session", Model: "native", Provider: "native-provider"}},
		{"native", "", Detection{Harness: "pi", SessionID: "session", Model: "native"}},
		{"", "native-provider", Detection{Harness: "pi", SessionID: "session", Model: "fallback", Provider: "contract-provider"}},
	} {
		env := map[string]string{"PI_CODING_AGENT": "true", "PI_SESSION_ID": "session", "PI_MODEL": tt.model, "PI_PROVIDER": tt.provider, "KATA_HARNESS_SESSION": "pi:session", "KATA_HARNESS_MODEL": "contract-provider/fallback"}
		got, outcome := Detect(lookup(env), nil, "")
		if outcome != Detected || got != tt.want {
			t.Fatalf("Detect() = (%+v, %v), want (%+v, Detected)", got, outcome, tt.want)
		}
	}
}
