package testfix

import (
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("KATA_TESTFIX_CHILD") == "1" {
		os.Exit(RunIsolated(m))
	}
	os.Exit(m.Run())
}

func TestRunIsolatedRemovesTemporaryArtifacts(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "artifacts.json")
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestRunIsolatedScratchChild$") //nolint:gosec // G204: launch this test executable from os.Executable.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "KATA_TEST_HOME=") && !strings.HasPrefix(value, "KATA_TESTFIX_CHILD=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "KATA_TESTFIX_CHILD=1", "KATA_TESTFIX_RECORD="+record)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated test process: %v\n%s", err, output)
	}
	data, err := os.ReadFile(record) //nolint:gosec // G304: record is inside this test's t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	var artifact string
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	// Keep a failing regression run from leaving its own synthetic files behind.
	t.Cleanup(func() { _ = os.RemoveAll(artifact) })
	if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test scratch survived process exit: %s (%v)", artifact, err)
	}
}

func TestRunIsolatedScratchChild(t *testing.T) {
	if os.Getenv("KATA_TESTFIX_CHILD") != "1" {
		t.Skip("subprocess fixture")
	}
	// Hook locks and other process-scoped artifacts use os.TempDir directly,
	// rather than t.TempDir's automatic cleanup.
	artifact, err := os.MkdirTemp("", "kata-hook-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifact, "config.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("KATA_TESTFIX_RECORD"), data, 0600); err != nil { //nolint:gosec // G703: the parent test supplies its owned result path.
		t.Fatal(err)
	}
}
