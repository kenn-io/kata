package hooks

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// absTestCommand returns an absolute command path valid on the host OS.
// filepath.IsAbs rejects Unix-style "/x" paths on Windows, so tests that need
// a valid absolute command must use a platform-appropriate one. Forward
// slashes are accepted by filepath.IsAbs on Windows (after a drive letter) and
// sidestep TOML backslash escaping.
func absTestCommand() string {
	if runtime.GOOS == "windows" {
		return "C:/Windows/System32/cmd.exe"
	}
	return "/usr/local/bin/notify"
}

// absTestCommandWithSpaces is absTestCommand for the internal-whitespace case.
func absTestCommandWithSpaces() string {
	if runtime.GOOS == "windows" {
		return "C:/Program Files/Some App/bin/notify"
	}
	return "/Applications/Some App/bin/notify"
}

func TestResolvedHook_MatchExact(t *testing.T) {
	h := ResolvedHook{Event: "issue.created", Match: func(s string) bool { return s == "issue.created" }}
	if !h.Match("issue.created") {
		t.Fatal("exact match failed")
	}
	if h.Match("issue.updated") {
		t.Fatal("exact match accepted wrong event")
	}
}

func TestConfig_ZeroValueIsZero(t *testing.T) {
	var c Config
	if c.PoolSize != 0 || c.QueueCap != 0 || c.OutputDiskCap != 0 {
		t.Fatal("zero-value Config should have zero ints")
	}
	if c.QueueFullLogInterval != 0 {
		t.Fatal("zero-value duration must be 0")
	}
}

func TestLoadedConfig_FieldsExist(t *testing.T) {
	lc := LoadedConfig{Snapshot: Snapshot{}, Config: Config{}, UnchangedTunables: nil}
	_ = lc.Snapshot
	_ = lc.Config
	_ = lc.UnchangedTunables
	rt := reflect.TypeOf(lc)
	if _, ok := rt.FieldByName("UnchangedTunables"); !ok {
		t.Fatal("UnchangedTunables field missing")
	}
}

// setupKataHome creates a temp directory and points $KATA_HOME at it.
func setupKataHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KATA_HOME", dir)
	return dir
}

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	dir := setupKataHome(t)
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertLoadError writes body, calls LoadStartup, and fails the test unless
// LoadStartup returns a non-nil error. msg describes what should have been
// rejected.
func assertLoadError(t *testing.T, body, msg string) {
	t.Helper()
	p := writeTOML(t, body)
	if _, err := LoadStartup(p); err == nil {
		t.Fatal(msg)
	}
}

// assertLoadOK writes body, calls LoadStartup, and fails the test if it
// returns an error. Returns the parsed LoadedConfig for further assertions.
func assertLoadOK(t *testing.T, body string) LoadedConfig {
	t.Helper()
	p := writeTOML(t, body)
	lc, err := LoadStartup(p)
	if err != nil {
		t.Fatal(err)
	}
	return lc
}

// defaultTestConfig is the baseline Config used as the "current" state in
// reload tests.
func defaultTestConfig() Config {
	return Config{PoolSize: 4, QueueCap: 1000}
}

func TestLoadStartup_FileMissing_EmptySnapshotDefaults(t *testing.T) {
	dir := setupKataHome(t)
	lc, err := LoadStartup(filepath.Join(dir, "hooks.toml"))
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if len(lc.Snapshot.Hooks) != 0 {
		t.Fatalf("missing file: hooks should be empty, got %d", len(lc.Snapshot.Hooks))
	}
	if lc.Config.PoolSize != 4 || lc.Config.QueueCap != 1000 {
		t.Fatalf("missing file: defaults wrong: pool=%d queue=%d", lc.Config.PoolSize, lc.Config.QueueCap)
	}
}

func TestLoadStartup_Malformed_ReturnsError(t *testing.T) {
	assertLoadError(t, "[[hook]]\nevent = ", "malformed TOML should error")
}

// TestLoadReload_Malformed_ReturnsError pins spec §4.6: SIGHUP / malformed →
// error, dispatcher keeps current Snapshot. The reload caller is responsible
// for not applying the result on error; this test just confirms the error
// surfaces.
func TestLoadReload_Malformed_ReturnsError(t *testing.T) {
	p := writeTOML(t, "[[hook]]\nevent = ")
	if _, err := LoadReload(p, defaultTestConfig()); err == nil {
		t.Fatal("malformed TOML on reload should error")
	}
}

func TestLoadReload_Missing_EmptySnapshotNoError(t *testing.T) {
	dir := setupKataHome(t)
	lc, err := LoadReload(filepath.Join(dir, "hooks.toml"), defaultTestConfig())
	if err != nil {
		t.Fatalf("missing file on reload: %v", err)
	}
	if len(lc.Snapshot.Hooks) != 0 {
		t.Fatal("missing reload should produce empty snapshot")
	}
}

func TestLoadReload_StartupOnlyDiff_PopulatesUnchangedTunables(t *testing.T) {
	p := writeTOML(t, `
[hooks]
pool_size = 8
queue_cap = 1000
[[hook]]
event   = "issue.created"
command = "true"
`)
	cur := Config{PoolSize: 4, QueueCap: 1000, OutputDiskCap: 100 << 20, RunsLogMaxBytes: 50 << 20, RunsLogKeep: 5, QueueFullLogInterval: 60 * time.Second}
	lc, err := LoadReload(p, cur)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range lc.UnchangedTunables {
		if strings.Contains(msg, "pool_size") && strings.Contains(msg, "8") && strings.Contains(msg, "4") {
			found = true
		}
	}
	if !found {
		t.Fatalf("UnchangedTunables missing pool_size diff: %v", lc.UnchangedTunables)
	}
}

func TestLoad_UnknownTOMLKey_ErrorsCitesKey(t *testing.T) {
	p := writeTOML(t, `
[hooks]
ploo_size = 8
`)
	_, err := LoadStartup(p)
	if err == nil || !strings.Contains(err.Error(), "ploo_size") {
		t.Fatalf("expected error citing 'ploo_size', got %v", err)
	}
}

func TestLoad_EventStarCreated_Error(t *testing.T) {
	assertLoadError(t, `
[[hook]]
event   = "*.created"
command = "true"
`, "event = *.created must be rejected")
}

func TestLoad_EventSyncResetRequired_Error(t *testing.T) {
	assertLoadError(t, `
[[hook]]
event   = "sync.reset_required"
command = "true"
`, "event = sync.reset_required must be rejected")
}

func TestLoad_CommandPaths(t *testing.T) {
	cases := []struct {
		cmd  string
		ok   bool
		desc string
	}{
		{absTestCommand(), true, "absolute"},
		{"notify", true, "bare name"},
		{"./foo", false, "dot-relative"},
		{"bin/foo", false, "embedded slash"},
		{"", false, "empty"},
		{".", false, "current-dir reference"},
		{"..", false, "parent-dir reference"},
		{" notify", false, "leading whitespace"},
		{"notify ", false, "trailing whitespace"},
		{"my command", false, "internal whitespace"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			body := "[[hook]]\nevent = \"issue.created\"\ncommand = \"" + c.cmd + "\"\n"
			if c.ok {
				assertLoadOK(t, body)
			} else {
				assertLoadError(t, body, "command "+c.cmd+" should be rejected")
			}
		})
	}
}

func TestLoad_RelativeWorkingDir_Error(t *testing.T) {
	assertLoadError(t, `
[[hook]]
event       = "issue.created"
command     = "true"
working_dir = "relative/path"
`, "relative working_dir must be rejected")
}

func TestLoad_KataPrefixedEnv_Error(t *testing.T) {
	assertLoadError(t, `
[[hook]]
event   = "issue.created"
command = "true"
[hook.env]
KATA_FOO = "x"
`, "[hook.env] keys matching ^KATA_ must be rejected")
}

func TestLoad_HookCountCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 257; i++ {
		b.WriteString("[[hook]]\nevent = \"issue.created\"\ncommand = \"true\"\n")
	}
	assertLoadError(t, b.String(), "257 [[hook]] entries must be rejected (cap 256)")
}

func TestLoad_SizeUnitsBinary(t *testing.T) {
	lc := assertLoadOK(t, `
[hooks]
output_disk_cap = "100k"
runs_log_max    = "1MB"
[[hook]]
event   = "issue.created"
command = "true"
`)
	if lc.Config.OutputDiskCap != 100*1024 {
		t.Fatalf("100k = %d, want %d", lc.Config.OutputDiskCap, 100*1024)
	}
	if lc.Config.RunsLogMaxBytes != 1024*1024 {
		t.Fatalf("1MB = %d, want %d", lc.Config.RunsLogMaxBytes, 1024*1024)
	}
}

func TestLoad_UserEnvSorted(t *testing.T) {
	lc := assertLoadOK(t, `
[[hook]]
event   = "issue.created"
command = "true"
[hook.env]
ZED   = "z"
ALPHA = "a"
MID   = "m"
`)
	got := lc.Snapshot.Hooks[0].UserEnv
	want := []string{"ALPHA=a", "MID=m", "ZED=z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UserEnv = %v, want %v", got, want)
	}
}

func TestLoad_DefaultWorkingDir_IsKataHome(t *testing.T) {
	dir := setupKataHome(t)
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte("[[hook]]\nevent = \"issue.created\"\ncommand = \"true\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lc, err := LoadStartup(path)
	if err != nil {
		t.Fatal(err)
	}
	if lc.Snapshot.Hooks[0].WorkingDir != dir {
		t.Fatalf("default working_dir = %q, want %q", lc.Snapshot.Hooks[0].WorkingDir, dir)
	}
}

func TestLoad_TimeoutBounds(t *testing.T) {
	cases := []struct {
		v    string
		ok   bool
		desc string
	}{
		{"30s", true, "default-ish"},
		{"0s", false, "zero rejected (open interval)"},
		{"5m", true, "max"},
		{"5m1s", false, "over max"},
		{"-1s", false, "negative"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			body := "[[hook]]\nevent=\"issue.created\"\ncommand=\"true\"\ntimeout=\"" + c.v + "\"\n"
			if c.ok {
				assertLoadOK(t, body)
			} else {
				assertLoadError(t, body, "timeout "+c.v+" should be rejected")
			}
		})
	}
}

func TestLoad_PoolSizeBounds(t *testing.T) {
	for _, body := range []string{"[hooks]\npool_size = 0\n", "[hooks]\npool_size = 17\n"} {
		assertLoadError(t, body, "body "+body+" should be rejected")
	}
}

// TestLoad_SizeAcceptsBareInteger pins that bare TOML integers (e.g.,
// output_disk_cap = 100) are valid byte values, alongside unit-suffixed
// strings like "100MB". Spec §4.2 lists both forms.
func TestLoad_SizeAcceptsBareInteger(t *testing.T) {
	lc := assertLoadOK(t, `
[hooks]
output_disk_cap = 12345
runs_log_max    = 67890
`)
	if lc.Config.OutputDiskCap != 12345 {
		t.Fatalf("output_disk_cap = %d, want 12345", lc.Config.OutputDiskCap)
	}
	if lc.Config.RunsLogMaxBytes != 67890 {
		t.Fatalf("runs_log_max = %d, want 67890", lc.Config.RunsLogMaxBytes)
	}
}

// TestLoad_SizeOverflow guards parseSize against int64 wraparound: a
// value times its unit must not exceed math.MaxInt64.
func TestLoad_SizeOverflow(t *testing.T) {
	// 9223372036854775807 / 1024 / 1024 ≈ 8796093022207 → adding "mb" puts
	// us above MaxInt64.
	assertLoadError(t, `
[hooks]
output_disk_cap = "9999999999999mb"
`, "massively oversize size value should be rejected")
}

// TestLoad_AbsolutePathWithSpaces pins that command validation accepts
// internal whitespace inside an absolute path (Windows "Program Files"
// or Unix custom dirs), while still rejecting bare names with spaces.
func TestLoad_AbsolutePathWithSpaces(t *testing.T) {
	assertLoadOK(t, "[[hook]]\nevent = \"issue.created\"\ncommand = \""+absTestCommandWithSpaces()+"\"\n")
}

// TestMatch_IssueStarOnlyKnown pins that the issue.* matcher rejects
// unknown event types like "issue.bogus" rather than fan-matching every
// string with the issue. prefix.
func TestMatch_IssueStarOnlyKnown(t *testing.T) {
	_, match, err := compileEventMatcher("issue.*")
	if err != nil {
		t.Fatal(err)
	}
	if !match("issue.created") {
		t.Fatal("issue.* should match issue.created")
	}
	if match("issue.bogus") {
		t.Fatal("issue.* must not match unknown issue.bogus")
	}
	if match("foo.created") {
		t.Fatal("issue.* must not match foo.created")
	}
}

// TestMatch_LinksChangedRecognized pins that the aggregated event added
// for kata#1 is in knownEventTypes — both the explicit form and the
// wildcard matchers must accept it. Without this, hook configs cannot
// subscribe to relationship changes routed through `kata edit`.
func TestMatch_LinksChangedRecognized(t *testing.T) {
	_, exact, err := compileEventMatcher("issue.links_changed")
	if err != nil {
		t.Fatal(err)
	}
	if !exact("issue.links_changed") {
		t.Fatal("explicit issue.links_changed matcher must match the event")
	}

	_, issueStar, err := compileEventMatcher("issue.*")
	if err != nil {
		t.Fatal(err)
	}
	if !issueStar("issue.links_changed") {
		t.Fatal("issue.* must match issue.links_changed")
	}

	_, allStar, err := compileEventMatcher("*")
	if err != nil {
		t.Fatal(err)
	}
	if !allStar("issue.links_changed") {
		t.Fatal("* must match issue.links_changed")
	}
}

// TestMatch_CloseThrottledRecognized pins that close.throttled audit
// events are in knownEventTypes: explicit subscription must validate,
// and the `*` wildcard must match. close.throttled deliberately lives
// outside the issue.* namespace (it's about a refused mutation, not the
// issue lifecycle), so issue.* must NOT match it.
func TestMatch_CloseThrottledRecognized(t *testing.T) {
	_, exact, err := compileEventMatcher("close.throttled")
	if err != nil {
		t.Fatal(err)
	}
	if !exact("close.throttled") {
		t.Fatal("explicit close.throttled matcher must match the event")
	}

	_, allStar, err := compileEventMatcher("*")
	if err != nil {
		t.Fatal(err)
	}
	if !allStar("close.throttled") {
		t.Fatal("* must match close.throttled")
	}

	_, issueStar, err := compileEventMatcher("issue.*")
	if err != nil {
		t.Fatal(err)
	}
	if issueStar("close.throttled") {
		t.Fatal("issue.* must not match close.throttled (out of namespace)")
	}
}
