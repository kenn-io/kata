package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	assertNever             = "assert" + ".Never"
	assertEventually        = "assert" + ".Eventually"
	assertEventuallyWithT   = "assert" + ".EventuallyWithT"
	requireNeverf           = "require" + ".Neverf"
	requireEventuallyf      = "require" + ".Eventuallyf"
	requireEventuallyWithTf = "require" + ".EventuallyWithTf"
)

func writeFixtureTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runFixture(t *testing.T, files map[string]string, allowed map[budgetKey]int) (int, string, map[string][]byte) {
	t.Helper()
	root := writeFixtureTree(t, files)
	before := make(map[string][]byte, len(files))
	for name := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		contents, err := os.ReadFile(path) //nolint:gosec // G304: path is a temporary fixture path.
		if err != nil {
			t.Fatal(err)
		}
		before[name] = contents
	}
	var stderr bytes.Buffer
	code := run([]string{root}, &stderr, allowed)
	assertFixturesUnchanged(t, root, before)
	return code, stderr.String(), before
}

func assertFixturesUnchanged(t *testing.T, root string, before map[string][]byte) {
	t.Helper()
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))) //nolint:gosec // G304: path is a temporary fixture path.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("fixture %s changed", name)
		}
	}
}

func TestRunReportsAllLiteralPollingAssertionsAndEvaluatesDurations(t *testing.T) {
	files := map[string]string{
		"fixture_test.go": `package fixture

import (
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	tm "time"
)

func TestLiteralBudgets(t *testing.T) {
	assertpkg.Never(t, func() bool { return false }, 999*tm.Millisecond, tm.Millisecond)
	requirepkg.Neverf(t, func() bool { return false }, 0, tm.Millisecond, "message")
	assertpkg.Eventually(t, func() bool { return false }, -tm.Millisecond, tm.Millisecond)
	requirepkg.Eventuallyf(t, func() bool { return false }, tm.Second/2, tm.Millisecond, "message")
	assertpkg.EventuallyWithT(t, func(*assertpkg.CollectT) {}, tm.Duration(50)*tm.Millisecond, tm.Millisecond)
	requirepkg.EventuallyWithTf(t, func(*assertpkg.CollectT) {}, tm.Second/2, tm.Millisecond, "message")
}
`,
	}
	code, got, _ := runFixture(t, files, nil)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; diagnostics: %s", code, got)
	}
	want := strings.Join([]string{
		"fixture_test.go:10: " + assertNever + " budget 999ms is below 1s in TestLiteralBudgets",
		"fixture_test.go:11: " + requireNeverf + " budget 0s is below 1s in TestLiteralBudgets",
		"fixture_test.go:12: " + assertEventually + " budget -1ms is below 1s in TestLiteralBudgets",
		"fixture_test.go:13: " + requireEventuallyf + " budget 500ms is below 1s in TestLiteralBudgets",
		"fixture_test.go:14: " + assertEventuallyWithT + " budget 50ms is below 1s in TestLiteralBudgets",
		"fixture_test.go:15: " + requireEventuallyWithTf + " budget 500ms is below 1s in TestLiteralBudgets",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("diagnostics:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunLeavesNamedAndLongBudgetsUnexamined(t *testing.T) {
	files := map[string]string{
		"fixture_test.go": `package fixture

import (
	requirepkg "github.com/stretchr/testify/require"
	"time"
)

func TestAcceptedBudgets(t *testing.T) {
	short := 50 * time.Millisecond
	requirepkg.Eventually(t, func() bool { return false }, short, time.Millisecond)
	requirepkg.Eventually(t, func() bool { return false }, time.Second, time.Millisecond)
	requirepkg.Eventually(t, func() bool { return false }, 2*time.Second, time.Millisecond)
	requirepkg.Eventually(t, func() bool { return false }, time.Until(time.Now()), time.Millisecond)
}
`,
	}
	code, got, _ := runFixture(t, files, nil)
	if code != 0 || got != "" {
		t.Fatalf("run() = %d, diagnostics %q; want clean", code, got)
	}
}

func TestRunIgnoresNonDirectCallsAndSkippedContent(t *testing.T) {
	files := map[string]string{
		"fixture_test.go": `package fixture

import (
	assertpkg "github.com/stretchr/testify/assert"
	"time"
)

type methods struct{}

func (methods) Never(any, any, time.Duration, time.Duration) {}

func TestIgnoredForms(t *testing.T) {
	// assertpkg.Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
	const source = "requirepkg.Never(t, fn, 50*time.Millisecond, time.Millisecond)"
	var receiver methods
	receiver.Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
	Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
	assertpkg.Never(t, func() bool { return false })
	assertpkg.Never(t, func() bool { return false }, 50*time.Millisecond)
}
`,
		"dot_test.go": `package fixture

import . "github.com/stretchr/testify/assert"

func TestDotImport(t *testing.T) {
	Never(t, func() bool { return false }, 50, 1)
}
`,
		"helper.go": `package fixture

	import requirepkg "github.com/stretchr/testify/require"
import "time"

func helper() {
	requirepkg.Never(nil, nil, 50*time.Millisecond, time.Millisecond)
}
`,
		"vendor/vendor_test.go": `package vendor

	import requirepkg "github.com/stretchr/testify/require"
import "time"

func TestSkipped(t *testing.T) {
	requirepkg.Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
}
`,
		"node_modules/node_test.go": `package node

	import requirepkg "github.com/stretchr/testify/require"
import "time"

func TestSkipped(t *testing.T) {
	requirepkg.Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
}
`,
		"testdata/data_test.go": `package data

	import requirepkg "github.com/stretchr/testify/require"
import "time"

func TestSkipped(t *testing.T) {
	requirepkg.Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
}
`,
		".hidden/hidden_test.go": `package hidden

	import requirepkg "github.com/stretchr/testify/require"
import "time"

func TestSkipped(t *testing.T) {
	requirepkg.Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
}
`,
		"_scratch/scratch_test.go": `package scratch

import requirepkg "github.com/stretchr/testify/require"
import "time"

func TestSkipped(t *testing.T) {
	requirepkg.Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
}
`,
		"unrelated_test.go": `package fixture

import (
	assertpkg "example.com/unrelated/assert"
	requirepkg "example.com/unrelated/require"
	"time"
)

func TestUnrelatedPackages(t *testing.T) {
	assertpkg.Never(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
	requirepkg.Eventually(t, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
}
`,
	}
	code, got, _ := runFixture(t, files, nil)
	if code != 0 || got != "" {
		t.Fatalf("run() = %d, diagnostics %q; want clean", code, got)
	}
}

func TestRunIgnoresShadowedIdentifiers(t *testing.T) {
	files := map[string]string{
		"fixture_test.go": `package fixture

import (
	assertpkg "github.com/stretchr/testify/assert"
	tm "time"
)

type fakeAssert struct{}

func (fakeAssert) Never(any, any, tm.Duration, tm.Duration) {}

type fakeTime struct {
	Millisecond tm.Duration
}

func TestShadowed(t *testing.T) {
	assertpkg := fakeAssert{}
	assertpkg.Never(t, func() bool { return false }, 50*tm.Millisecond, tm.Millisecond)
}

func TestShadowedTime(t *testing.T) {
	tm := fakeTime{Millisecond: 1}
	assertpkg.Never(t, func() bool { return false }, 50*tm.Millisecond, tm.Millisecond)
}
`,
	}
	code, got, _ := runFixture(t, files, nil)
	if code != 0 || got != "" {
		t.Fatalf("run() = %d, diagnostics %q; want clean", code, got)
	}
}

func TestRunAcceptsAndCountsExactAllowances(t *testing.T) {
	files := map[string]string{
		"internal/provider_test.go": `package provider

import (
	assertpkg "github.com/stretchr/testify/assert"
	"time"
)

func TestProvider(t *testing.T) {
	assertpkg.Never(t, func() bool { return false }, 20*time.Millisecond, time.Millisecond)
}
`,
	}
	allowed := map[budgetKey]int{{
		Path:      "internal/provider_test.go",
		Function:  "TestProvider",
		Assertion: assertNever,
		Duration:  20 * time.Millisecond,
	}: 1}
	code, got, _ := runFixture(t, files, allowed)
	if code != 0 || got != "" {
		t.Fatalf("run() = %d, diagnostics %q; want clean", code, got)
	}
}

func TestRunReportsExtraAllowanceOccurrences(t *testing.T) {
	files := map[string]string{
		"provider_test.go": `package provider

import (
	assertpkg "github.com/stretchr/testify/assert"
	"time"
)

func TestProvider(t *testing.T) {
	assertpkg.Never(t, func() bool { return false }, 20*time.Millisecond, time.Millisecond)
	assertpkg.Never(t, func() bool { return false }, 20*time.Millisecond, time.Millisecond)
}
`,
	}
	allowed := map[budgetKey]int{{
		Path:      "provider_test.go",
		Function:  "TestProvider",
		Assertion: assertNever,
		Duration:  20 * time.Millisecond,
	}: 1}
	code, got, _ := runFixture(t, files, allowed)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; diagnostics: %s", code, got)
	}
	want := "provider_test.go:10: " + assertNever + " budget 20ms is an extra occurrence; allowance permits 1\n"
	if got != want {
		t.Fatalf("diagnostics %q, want %q", got, want)
	}
}

func TestRunReportsChangedAndStaleAllowances(t *testing.T) {
	files := map[string]string{
		"provider_test.go": `package provider

import (
	assertpkg "github.com/stretchr/testify/assert"
	"time"
)

func TestProvider(t *testing.T) {
	assertpkg.Never(t, func() bool { return false }, 30*time.Millisecond, time.Millisecond)
}
`,
	}
	allowed := map[budgetKey]int{{
		Path:      "provider_test.go",
		Function:  "TestProvider",
		Assertion: assertNever,
		Duration:  20 * time.Millisecond,
	}: 1}
	code, got, _ := runFixture(t, files, allowed)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; diagnostics: %s", code, got)
	}
	want := strings.Join([]string{
		"provider_test.go:9: " + assertNever + " budget 30ms is below 1s in TestProvider",
		"provider_test.go: allowance " + assertNever + " budget 20ms for TestProvider is stale",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("diagnostics:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunReportsStaleAllowances(t *testing.T) {
	root := writeFixtureTree(t, map[string]string{"provider_test.go": "package provider\n"})
	var stderr bytes.Buffer
	allowed := map[budgetKey]int{{
		Path:      "provider_test.go",
		Function:  "TestProvider",
		Assertion: assertNever,
		Duration:  20 * time.Millisecond,
	}: 1}
	if code := run([]string{root}, &stderr, allowed); code != 1 {
		t.Fatalf("run() = %d, want 1; diagnostics: %s", code, stderr.String())
	}
	want := "provider_test.go: allowance " + assertNever + " budget 20ms for TestProvider is stale\n"
	if stderr.String() != want {
		t.Fatalf("diagnostics %q, want %q", stderr.String(), want)
	}
}

func TestRunReturnsUsageAndInputErrors(t *testing.T) {
	root := writeFixtureTree(t, map[string]string{"fixture_test.go": "package fixture\n"})
	file := filepath.Join(root, "fixture_test.go")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "too many arguments", args: []string{"one", "two"}, want: "usage: timingbudgetcheck [directory]\n"},
		{name: "missing root", args: []string{filepath.Join(root, "missing")}, want: "timingbudgetcheck:"},
		{name: "file root", args: []string{file}, want: "timingbudgetcheck:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(tt.args, &stderr, nil); code != 2 {
				t.Fatalf("run() = %d, want 2; diagnostics: %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("diagnostics %q do not contain %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestRunReturnsParseErrorsAndDoesNotWriteSource(t *testing.T) {
	files := map[string]string{
		"bad_test.go": "package fixture\nfunc TestBroken(t *testing.T) {\n",
	}
	root := writeFixtureTree(t, files)
	before, err := os.ReadFile(filepath.Join(root, "bad_test.go")) //nolint:gosec // G304: path is a temporary fixture path.
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run([]string{root}, &stderr, nil); code != 2 {
		t.Fatalf("run() = %d, want 2; diagnostics: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "timingbudgetcheck: bad_test.go:") {
		t.Fatalf("diagnostics %q do not identify the bad file", stderr.String())
	}
	after, err := os.ReadFile(filepath.Join(root, "bad_test.go")) //nolint:gosec // G304: path is a temporary fixture path.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("checker changed malformed source")
	}
}
