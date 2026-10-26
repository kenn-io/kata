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

	"hegel.dev/go/hegel"
)

func TestPiChildDoesNotTakeOverLeadAttention(t *testing.T) {
	testPiChildAttention(t, "lead-ref", "lead-ref", "https://daemon.example", "https://daemon.example")
}

func attentionHostFixture(t *testing.T) func(*hegel.T, any, any) {
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
	return func(ht *hegel.T, input, verdict any) {
		// Node readline treats Unicode JS line separators as record boundaries.
		data, err := json.Marshal(input, jsontext.EscapeForJS(true))
		if err != nil {
			ht.Fatal(err)
		}
		if _, err := fmt.Fprintln(stdin, string(data)); err != nil {
			ht.Fatal(err)
		}
		response, err := decoder.ReadValue()
		if err != nil {
			ht.Fatalf("missing fixture verdict: %v", err)
		}
		if err := json.Unmarshal(response, verdict); err != nil {
			ht.Fatal(err)
		}
	}
}

func TestAttentionHostMarkerProperty(t *testing.T) {
	exchange := attentionHostFixture(t)
	hegel.Test(t, func(ht *hegel.T) {
		markerPid := hegel.Draw(ht, hegel.Integers(1, 2147483647))
		ownPid := hegel.Draw(ht, hegel.Integers(1, 2147483647))
		if hegel.Draw(ht, hegel.Booleans()) {
			ownPid = markerPid
		}
		identities := hegel.SampledFrom([]string{
			`{"daemon":"local","project":42,"issue":"lead"}`,
			`{"daemon":"remote","project":42,"issue":"lead"}`,
			`{"daemon":"local","project":43,"issue":"lead"}`,
		})
		markerIdentity, identity := hegel.Draw(ht, identities), hegel.Draw(ht, identities)
		alive := hegel.Draw(ht, hegel.Booleans())
		malformed := hegel.Draw(ht, hegel.Booleans())
		input := map[string]any{
			"marker": map[string]any{"pid": markerPid, "identity": markerIdentity},
			"ownPid": ownPid, "identity": identity, "alive": alive, "malformed": malformed,
			"denied": hegel.Draw(ht, hegel.Booleans()),
		}
		if malformed && markerPid%22 == 0 {
			delete(input, "marker")
		}
		var verdict struct {
			Skip bool `json:"skip"`
		}
		exchange(ht, input, &verdict)
		want := !malformed && markerPid != ownPid && alive && markerIdentity == identity
		if verdict.Skip != want {
			ht.Fatalf("skip = %v, want %v for %+v", verdict.Skip, want, input)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestAttentionHostClaimReleaseProperty(t *testing.T) {
	exchange := attentionHostFixture(t)
	hegel.Test(t, func(ht *hegel.T) {
		pid := hegel.Draw(ht, hegel.Integers(1, 2147483647))
		identity := hegel.Draw(ht, hegel.Text())
		replacement := hegel.Draw(ht, hegel.SampledFrom([]string{"own", "pid", "identity", "reordered", "extended", "malformed", "absent"}))
		var verdict struct {
			PID       int    `json:"pid"`
			Identity  string `json:"identity"`
			Released  bool   `json:"released"`
			Unchanged bool   `json:"unchanged"`
			KataRef   string `json:"kataRef"`
		}
		exchange(ht, map[string]any{"operation": "claim-release", "ownPid": pid, "identity": identity, "replacement": replacement}, &verdict)
		released := replacement == "own" || replacement == "absent"
		if verdict.PID != pid || verdict.Identity != identity || verdict.KataRef != "preserved-ref" || verdict.Released != released || (!released && !verdict.Unchanged) {
			ht.Fatalf("claim/release changed the wrong environment value: replacement=%s verdict=%+v", replacement, verdict)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestPiChildWithDifferentRefTracksOwnIssue(t *testing.T) {
	testPiChildAttention(t, "lead-ref", "other-ref", "https://daemon.example", "https://daemon.example")
}

func TestPiChildWithEquivalentQualifiedRefDoesNotTakeOverLeadAttention(t *testing.T) {
	testPiChildAttention(t, "abc4", "spoke-project#abc4", "https://daemon.example", "https://daemon.example")
}

func TestPiChildWithEquivalentBareRefDoesNotTakeOverLeadAttention(t *testing.T) {
	testPiChildAttention(t, "spoke-project#abc4", "abc4", "https://daemon.example", "https://daemon.example")
}

func TestPiChildWithSameShortIDInDifferentProjectTracksOwnIssue(t *testing.T) {
	testPiChildAttention(t, "spoke-project#abc4", "hub-project#abc4", "https://daemon.example", "https://daemon.example")
}

func TestPiChildWithSameIssueOnDifferentDaemonTracksOwnIssue(t *testing.T) {
	testPiChildAttention(t, "spoke-project#abc4", "spoke-project#abc4", "https://daemon-one.example", "https://daemon-two.example")
}

func TestPiChildWithEquivalentTCPRoutesDoesNotTakeOverLeadAttention(t *testing.T) {
	testPiChildAttention(t, "abc4", "spoke-project#abc4", "127.0.0.1:7777", "http://127.0.0.1:7777")
}

func TestPiChildWithSameUnixSocketDoesNotTakeOverLeadAttention(t *testing.T) {
	testPiChildAttention(t, "abc4", "spoke-project#abc4", "unix:///tmp/kata.sock", "unix:///tmp/kata.sock")
}

func TestPiChildWithDifferentUnixSocketsTracksOwnIssue(t *testing.T) {
	testPiChildAttention(t, "spoke-project#abc4", "spoke-project#abc4", "unix:///tmp/lead.sock", "unix:///tmp/child.sock")
}

func TestPiNativeReloadRestoresMarkerlessStartedBaseline(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the attention host fixture")
	}
	opts := piTestOptions(t)
	opts.Attention = true
	opts.Contract = false
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(t.TempDir(), "pi.mjs")
	if err := os.WriteFile(extension, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "testdata/agent-hooks/pi-markerless-reload.mjs", extension, opts.Dir) //nolint:gosec // G204: fixed fixture executes a generated adapter in a temporary directory.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Pi markerless reload: %v\n%s", err, output)
	}
}

func TestPiNativeRetiredIdentityLookupCannotStartAfterReplacement(t *testing.T) {
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
	command := exec.Command(node, "testdata/agent-hooks/pi-delayed-identity.mjs", extension, opts.Dir) //nolint:gosec // G204: fixed fixture executes a generated adapter in a temporary directory.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Pi retired identity lookup: %v\n%s", err, output)
	}
}

func testPiChildAttention(t *testing.T, leadRef, childRef, leadServer, childServer string) {
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
	command := exec.Command(node, "testdata/agent-hooks/pi-child.mjs", extension, opts.Dir, leadRef, childRef, leadServer, childServer) //nolint:gosec // G204: fixed fixture executes a generated adapter in temporary directories.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Pi child attention: %v\n%s", err, output)
	}
}
