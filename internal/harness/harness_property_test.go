package harness

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
)

// Scrubbing removes canonical identity entries and preserves all other entries
// in their original order, including duplicates, malformed entries and values.
func TestPropertyStripEnvKeepsOthers(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		caseInsensitive := hegel.Draw(ht, hegel.Booleans())
		var environ, want []string
		for _, entry := range hegel.Draw(ht, hegel.Lists(hegel.Text())) {
			entry = "OTHER_" + entry
			environ = append(environ, entry)
			want = append(want, entry)
		}
		for _, key := range EnvKeys() {
			value := hegel.Draw(ht, hegel.Text())
			environ = append(environ, key+"="+value, key+"=", key)
			variants := []string{strings.ToLower(key) + "=" + value, strings.ToLower(key[:1]) + key[1:] + "=" + value}
			environ = append(environ, variants...)
			if !caseInsensitive {
				want = append(want, variants...)
			}
			near := key + "_EXTRA=" + value
			environ = append(environ, near, near)
			want = append(want, near, near)
		}
		if got := stripEnv(environ, caseInsensitive); !slices.Equal(got, want) {
			ht.Fatalf("StripEnv(%q) = %q, want %q", environ, got, want)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

// Spec property 5: unrelated keys cannot change any detection field or outcome.
func TestPropertyIrrelevantEnvIgnored(t *testing.T) {
	seeds := []map[string]string{
		{},
		{"CLAUDE_CODE_SESSION_ID": sessionID},
		{"CODEX_THREAD_ID": sessionID},
		{"CODEX_SESSION_ID": sessionID},
		{"CODEX_THREAD_ID": sessionID, "CODEX_SESSION_ID": "another-thread"},
		{"PI_CODING_AGENT": "true", "PI_SESSION_ID": sessionID},
		{"OMPCODE": "1", "PI_SESSION_ID": sessionID},
		{"AGENT": "amp"},
		{"OPENCLAW_SHELL": "exec"},
		{"CURSOR_AGENT": "1"},
		{"GEMINI_CLI": "1"},
		{"QWEN_CODE": "1"},
		{"PI_CODING_AGENT": "true", "OMPCODE": "1"},
	}
	hegel.Test(t, func(ht *hegel.T) {
		base := maps.Clone(hegel.Draw(ht, hegel.SampledFrom(seeds)))
		// Generate model and session content independently of fixed markers.
		for _, key := range []string{"PI_MODEL", "PI_PROVIDER", "PI_SESSION_ID", "KATA_HARNESS_SESSION", "KATA_HARNESS_MODEL"} {
			base[key] = hegel.Draw(ht, hegel.Text())
		}
		want, wantOutcome := Detect(lookup(base), nil, "")
		extended := maps.Clone(base)
		noise := hegel.Draw(ht, hegel.Maps(hegel.Text(), hegel.Text()))
		for key, value := range noise {
			if !slices.Contains(EnvKeys(), key) {
				extended[key] = value
			}
		}
		got, outcome := Detect(lookup(extended), nil, "")
		if outcome != wantOutcome || got != want {
			ht.Fatalf("irrelevant env changed (%+v, %v) to (%+v, %v); noise=%q", want, wantOutcome, got, outcome, noise)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

// Spec property 6: ancestry picks among every present marker family, and the
// selected harness retains its own session source rather than an outer one.
func TestPropertyNestingUsesNearest(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		sharedSession := hegel.Draw(ht, hegel.Text())
		codexSession := "thread-" + hegel.Draw(ht, hegel.Text())
		families := []struct {
			name, key, value, session string
		}{
			{"claude", "CLAUDE_CODE_SESSION_ID", sessionID, sessionID},
			{"codex", "CODEX_THREAD_ID", codexSession, codexSession},
			{"pi", "PI_CODING_AGENT", "true", sharedSession},
			{"omp", "OMPCODE", "1", sharedSession},
			{"amp", "AGENT", "amp", ""},
			{"openclaw", "OPENCLAW_SHELL", "exec", ""},
			{"cursor", "CURSOR_AGENT", "1", ""},
			{"gemini", "GEMINI_CLI", "1", ""},
			{"qwen", "QWEN_CODE", "1", ""},
		}
		subset := hegel.Draw(ht, hegel.Integers(1, (1<<len(families))-1))
		env := map[string]string{"PI_SESSION_ID": sharedSession}
		var candidates []string
		var sessions []string
		for i, family := range families {
			if subset&(1<<i) != 0 {
				env[family.key] = family.value
				candidates = append(candidates, family.name)
				sessions = append(sessions, family.session)
			}
		}
		winner := hegel.Draw(ht, hegel.Integers(0, len(candidates)-1))
		probe := ancestryFunc(func(gotCandidates []string) (string, string, bool) {
			if len(gotCandidates) != len(candidates) {
				ht.Fatalf("ancestry candidates = %q, want %q", gotCandidates, candidates)
			}
			for _, candidate := range candidates {
				if !slices.Contains(gotCandidates, candidate) {
					ht.Fatalf("ancestry candidates = %q, missing %q", gotCandidates, candidate)
				}
			}
			return candidates[winner], "42:123456", true
		})
		want := Detection{Harness: candidates[winner], SessionID: sessions[winner]}
		got, outcome := Detect(lookup(env), probe, "")
		if outcome != Detected || got != want {
			ht.Fatalf("Detect(%q) = (%+v, %v), want (%+v, Detected)", candidates, got, outcome, want)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

// Spec property 8: exact harness/session binding gates contract model data;
// a nonempty native Pi model retains its native provider as well.
func TestPropertyContractBinding(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		harness := hegel.Draw(ht, hegel.SampledFrom(Names()))
		session := hegel.Draw(ht, hegel.Text())
		rawModel := hegel.Draw(ht, hegel.Text())
		nativeModel := hegel.Draw(ht, hegel.Text())
		nativeProvider := hegel.Draw(ht, hegel.Text())
		env := map[string]string{
			"PI_SESSION_ID": session, "CODEX_THREAD_ID": session,
			"CLAUDE_CODE_SESSION_ID": session,
			"PI_MODEL":               nativeModel, "PI_PROVIDER": nativeProvider,
			"KATA_HARNESS_MODEL": rawModel,
		}
		if harness != "claude" && harness != "codex" && harness != "pi" && harness != "omp" {
			session = ""
		}
		binding := harness + ":" + session
		matched := hegel.Draw(ht, hegel.Booleans())
		if !matched {
			// Change either side of the binding; preserve arbitrary session text.
			if hegel.Draw(ht, hegel.Booleans()) {
				binding = "other-" + binding
			} else {
				binding += "-other"
			}
		}
		env["KATA_HARNESS_SESSION"] = binding
		want := Detection{Harness: harness, SessionID: session}
		if harness == "pi" {
			want.Model, want.Provider = nativeModel, nativeProvider
		}
		if matched && want.Model == "" {
			want.Model, want.Provider = rawModel, ""
			if provider, model, ok := strings.Cut(rawModel, "/"); ok {
				want.Provider, want.Model = provider, model
			}
		}
		got, outcome := Detect(lookup(env), nil, harness)
		if outcome != Detected || got != want {
			ht.Fatalf("Detect(hint=%q, binding=%q) = (%+v, %v), want (%+v, Detected)", harness, binding, got, outcome, want)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
