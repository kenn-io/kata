package harness

import (
	"maps"
	"regexp"
	"slices"
)

type modelSource int

const (
	modelUnknown modelSource = iota
	modelNative
	modelContract
	modelHostRecord
)

// marker keeps each harness's native signals and executable identity together.
// Runtime names are script wrappers: argv[1]'s basename must match the harness.
type marker struct {
	name               string
	markerKeys         []string
	matches            func(string) bool
	sessionKeys        []string
	modelSource        modelSource
	modelKey           string
	providerKey        string
	executables        []string
	runtimes           []string
	executablePrefixes []string
}

var sessionUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var markers = []marker{
	{
		name: "claude", markerKeys: []string{"CLAUDE_CODE_SESSION_ID"}, matches: sessionUUID.MatchString,
		sessionKeys: []string{"CLAUDE_CODE_SESSION_ID"}, modelSource: modelContract,
		executables: []string{"claude"},
	},
	{
		name: "codex", markerKeys: []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID"}, matches: nonempty,
		sessionKeys: []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID"}, modelSource: modelHostRecord,
		executables: []string{"codex"},
	},
	{
		name: "pi", markerKeys: []string{"PI_CODING_AGENT"}, matches: equals("true"),
		sessionKeys: []string{"PI_SESSION_ID"}, modelSource: modelNative, modelKey: "PI_MODEL", providerKey: "PI_PROVIDER",
		executables: []string{"pi"}, runtimes: []string{"node", "bun"},
	},
	{
		name: "omp", markerKeys: []string{"OMPCODE"}, matches: equals("1"),
		sessionKeys: []string{"PI_SESSION_ID"}, modelSource: modelContract,
		executables: []string{"omp"},
	},
	{name: "amp", markerKeys: []string{"AGENT"}, matches: equals("amp"), executables: []string{"amp"}},
	{name: "openclaw", markerKeys: []string{"OPENCLAW_SHELL"}, matches: equals("exec"), executables: []string{"openclaw"}},
	{name: "cursor", markerKeys: []string{"CURSOR_AGENT"}, matches: equals("1"), executables: []string{"cursor-agent"}},
	{name: "gemini", markerKeys: []string{"GEMINI_CLI"}, matches: equals("1"), executables: []string{"gemini"}},
	{name: "qwen", markerKeys: []string{"QWEN_CODE"}, matches: equals("1"), executables: []string{"qwen"}},
	{name: "muse", modelSource: modelHostRecord, executables: []string{"muse"}, executablePrefixes: []string{"muse-bin-"}},
}

func nonempty(value string) bool { return value != "" }

func equals(want string) func(string) bool {
	return func(value string) bool { return value == want }
}

// EnvKeys returns every marker, session, model and contract key in sorted order.
// Callers may modify the returned slice without changing the marker registry.
func EnvKeys() []string {
	keys := map[string]struct{}{
		"KATA_HARNESS_SESSION": {},
		"KATA_HARNESS_MODEL":   {},
	}
	for _, m := range markers {
		for _, group := range [][]string{m.markerKeys, m.sessionKeys, {m.modelKey, m.providerKey}} {
			for _, key := range group {
				if key != "" {
					keys[key] = struct{}{}
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(keys))
}

// Names returns the canonical harness names in sorted order.
func Names() []string {
	names := make([]string, 0, len(markers))
	for _, m := range markers {
		names = append(names, m.name)
	}
	slices.Sort(names)
	return names
}
