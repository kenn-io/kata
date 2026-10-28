package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"
)

func TestPiChildDoesNotTakeOverLeadAttention(t *testing.T) {
	testPiChildAttention(t, "lead-ref", "lead-ref", "unix:///tmp/kata.sock", "unix:///tmp/kata.sock")
}

func attentionHostFixture(t *testing.T) func(*rapid.T, any, any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the attention host fixture")
	}
	guard := filepath.Join(t.TempDir(), "guard.mjs")
	if err := os.WriteFile(guard, []byte(nativeAttentionHostGuardJS+"\nexport {kataAttentionHostSkip, kataAttentionHostClaim, kataAttentionHostRelease};\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "testdata/agent-hooks/attention-host.mjs", guard) //nolint:gosec // G204: fixed fixture imports the production guard from a temporary file.
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if err := command.Wait(); err != nil {
			t.Errorf("attention host fixture: %v\n%s", err, &stderr)
		}
	})
	decoder := jsontext.NewDecoder(stdout)
	return func(rt *rapid.T, input, verdict any) {
		// Node readline treats Unicode JS line separators as record boundaries.
		data, err := json.Marshal(input, jsontext.EscapeForJS(true))
		if err != nil {
			rt.Fatal(err)
		}
		if _, err := fmt.Fprintln(stdin, string(data)); err != nil {
			rt.Fatal(err)
		}
		response, err := decoder.ReadValue()
		if err != nil {
			rt.Fatalf("missing fixture verdict: %v", err)
		}
		if err := json.Unmarshal(response, verdict); err != nil {
			rt.Fatal(err)
		}
	}
}

func TestAttentionHostMarkerProperty(t *testing.T) {
	exchange := attentionHostFixture(t)
	rapid.Check(t, func(rt *rapid.T) {
		markerPid := rapid.IntRange(1, 2147483647).Draw(rt, "marker pid")
		ownPid := rapid.IntRange(1, 2147483647).Draw(rt, "own pid")
		if rapid.Bool().Draw(rt, "same pid") {
			ownPid = markerPid
		}
		identities := rapid.SampledFrom([]string{
			`{"daemon":"local","project":42,"issue":"lead"}`,
			`{"daemon":"remote","project":42,"issue":"lead"}`,
			`{"daemon":"local","project":43,"issue":"lead"}`,
		})
		markerIdentity, identity := identities.Draw(rt, "marker identity"), identities.Draw(rt, "identity")
		alive := rapid.Bool().Draw(rt, "alive")
		malformed := rapid.Bool().Draw(rt, "malformed")
		input := map[string]any{
			"marker": map[string]any{"pid": markerPid, "identity": markerIdentity},
			"ownPid": ownPid, "identity": identity, "alive": alive, "malformed": malformed,
			"denied": rapid.Bool().Draw(rt, "denied"),
		}
		if malformed && markerPid%22 == 0 {
			delete(input, "marker")
		}
		var verdict struct {
			Skip bool `json:"skip"`
		}
		exchange(rt, input, &verdict)
		want := !malformed && markerPid != ownPid && alive && markerIdentity == identity
		if verdict.Skip != want {
			rt.Fatalf("skip = %v, want %v for %+v", verdict.Skip, want, input)
		}
	})
}

func TestAttentionHostClaimReleaseProperty(t *testing.T) {
	exchange := attentionHostFixture(t)
	rapid.Check(t, func(rt *rapid.T) {
		pid := rapid.IntRange(1, 2147483647).Draw(rt, "pid")
		identity := rapid.String().Draw(rt, "identity")
		replacement := rapid.SampledFrom([]string{"own", "pid", "identity", "reordered", "extended", "malformed", "absent"}).Draw(rt, "replacement")
		var verdict struct {
			PID       int    `json:"pid"`
			Identity  string `json:"identity"`
			Released  bool   `json:"released"`
			Unchanged bool   `json:"unchanged"`
			KataRef   string `json:"kataRef"`
		}
		exchange(rt, map[string]any{"operation": "claim-release", "ownPid": pid, "identity": identity, "replacement": replacement}, &verdict)
		released := replacement == "own" || replacement == "absent"
		if verdict.PID != pid || verdict.Identity != identity || verdict.KataRef != "preserved-ref" || verdict.Released != released || (!released && !verdict.Unchanged) {
			rt.Fatalf("claim/release changed the wrong environment value: replacement=%s verdict=%+v", replacement, verdict)
		}
	})
}

func TestPiChildWithDifferentRefTracksOwnIssue(t *testing.T) {
	testPiChildAttention(t, "lead-ref", "other-ref", "unix:///tmp/kata.sock", "unix:///tmp/kata.sock")
}

// The workspace chooses the project for a bare ref, so a child elsewhere may
// track another issue on the same daemon.
func TestPiChildInDifferentDirectoryTracksOwnIssue(t *testing.T) {
	testPiChildAttention(t, "lead-ref", "lead-ref", "unix:///tmp/kata.sock", "unix:///tmp/kata.sock", "nested")
}

// The child's environment or workspace config can select another daemon
// without KATA_SERVER, for example through KATA_HOME.
func TestPiChildOnDifferentDaemonTracksOwnIssue(t *testing.T) {
	testPiChildAttention(t, "lead-ref", "lead-ref", "unix:///tmp/lead.sock", "unix:///tmp/child.sock")
}

func TestPiChildWithUnknownDaemonTracksOwnIssue(t *testing.T) {
	testPiChildAttention(t, "lead-ref", "lead-ref", "unix:///tmp/kata.sock", "")
}

func testPiChildAttention(t *testing.T, leadRef, childRef, leadDaemon, childDaemon string, childSubdir ...string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the attention host fixture")
	}
	opts := piTestOptions(t)
	opts.Attention = true
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(t.TempDir(), "pi.mjs")
	if err := os.WriteFile(extension, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, append([]string{"testdata/agent-hooks/pi-child.mjs", extension, opts.Dir, leadRef, childRef, leadDaemon, childDaemon}, childSubdir...)...) //nolint:gosec // G204: fixed fixture executes a generated adapter in temporary directories.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Pi child attention: %v\n%s", err, output)
	}
}
