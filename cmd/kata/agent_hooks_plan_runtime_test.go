package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"hegel.dev/go/hegel"
)

func planNode(t *testing.T, fixture string, args ...string) *exec.Cmd {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	core, err := filepath.Abs("agenthookjs/plan_core.js")
	require.NoError(t, err)
	cmd := exec.Command(node, append([]string{filepath.Join("testdata/agent-hooks", fixture), core}, args...)...) //nolint:gosec // G204: fixture names come from fixed Go test cases and arguments are test-controlled.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "KATA_") ||
			strings.HasPrefix(entry, "KATA_HTTP_TIMEOUT=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	return cmd
}
func runPlanNode(t *testing.T, cmd *exec.Cmd) []byte {
	t.Helper()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return out
}
func planTestBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "kata")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", binary, ".") //nolint:gosec // G204: build the local Kata test executable used by the fixtures.
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return binary
}
func TestPlanCoreOwnership(t *testing.T) {
	cmd := planNode(t, "plan-ownership.mjs")
	cmd.Env = append(cmd.Env, "KATA_TEST_EXECUTABLE="+planTestBinary(t))
	t.Log(string(runPlanNode(t, cmd)))
}
func TestPlanCoreTodoOps(t *testing.T) {
	cmd := planNode(t, "plan-core.mjs")
	cmd.Env = append(cmd.Env, "KATA_TEST_EXECUTABLE="+planTestBinary(t))
	t.Log(string(runPlanNode(t, cmd)))
}
func TestPlanHelpers(t *testing.T)       { runPlanNode(t, planNode(t, "plan-helpers.mjs")) }
func TestPlanShellCommands(t *testing.T) { runPlanNode(t, planNode(t, "plan-shell.mjs")) }
func TestPlanDaemonFloor(t *testing.T)   { runPlanNode(t, planNode(t, "plan-contract.mjs", "identity")) }
func TestPlanParentErrorClassification(t *testing.T) {
	runPlanNode(t, planNode(t, "plan-parent-errors.mjs"))
}
func TestPlanParent(t *testing.T) { runPlanNode(t, planNode(t, "plan-contract.mjs", "parent")) }

// Keep one interpreter per property, including shrinking. Requests and verdicts
// cross a JSON-lines channel; every draw still gets its own exact assertion.
func planPropertyFixture(t *testing.T, fixture, mode string) func(*hegel.T, any, any) {
	t.Helper()
	cmd := planNode(t, fixture, mode, "stream")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("plan property interpreter: %v: %s", err, &stderr)
		}
	})
	decoder := jsontext.NewDecoder(stdout)
	return func(ht *hegel.T, input, output any) {
		data, err := json.Marshal(input, jsontext.EscapeForJS(true))
		if err != nil {
			ht.Fatal(err)
		}
		if _, err = fmt.Fprintln(stdin, string(data)); err != nil {
			ht.Fatal(err)
		}
		value, err := decoder.ReadValue()
		if err != nil {
			ht.Fatalf("plan property verdict: %v: %s", err, &stderr)
		}
		if err = json.Unmarshal(value, output); err != nil {
			ht.Fatal(err)
		}
	}
}
func TestPlanClassificationProperty(t *testing.T) {
	exchange := planPropertyFixture(t, "plan-contract.mjs", "classification")
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Text())
		handle, lead := hegel.Draw(ht, hegel.Text()), hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Booleans()) {
			lead = ""
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			handle = hegel.Draw(ht, hegel.SampledFrom([]string{"", "omp", "omp.x", "other.worker"}))
		}
		meta := map[string]any{"teammate": handle}
		delegate := hegel.Draw(ht, hegel.Booleans())
		if delegate {
			meta["omp.subagent_id"] = id
		}
		var got []struct {
			Classification string `json:"classification"`
		}
		exchange(ht, []any{map[string]any{"metadata": meta, "lead": lead, "id": id}}, &got)
		require.Len(ompPropertyT{ht}, got, 1)
		want := "other"
		if delegate {
			want = "delegate"
		} else if handle == lead || lead == "" && (handle == "omp" || strings.HasPrefix(handle, "omp.")) {
			want = "lead"
		}
		require.Equal(ompPropertyT{ht}, want, got[0].Classification)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
func TestPlanParentEvidenceProperty(t *testing.T) {
	exchange := planPropertyFixture(t, "plan-contract.mjs", "parent")
	hegel.Test(t, func(ht *hegel.T) {
		sequences := hegel.Draw(ht, hegel.Lists(hegel.Lists(hegel.Text())))
		// Draw the full domain; bound only the work materialized for the fake store.
		if len(sequences) > 12 {
			sequences = sequences[:12]
		}
		if len(sequences) == 0 {
			sequences = [][]string{{"initial"}}
		}
		histories := [][][]string{}
		want := map[string]bool{}
		for _, sequence := range sequences {
			if len(sequence) == 0 {
				sequence = []string{"initial"}
			}
			if len(sequence) > 16 {
				sequence = sequence[len(sequence)-16:]
			}
			history := [][]string{}
			for _, value := range sequence {
				encoded, _ := json.Marshal(value)
				history = append(history, []string{"test:command " + string(encoded)})
			}
			histories = append(histories, history)
			want[history[len(history)-1][0]] = true
		}
		var got [][]string
		exchange(ht, []any{map[string]any{"histories": histories}}, &got)
		require.Len(ompPropertyT{ht}, got, 1)
		expected := []string{}
		for proof := range want {
			expected = append(expected, proof)
		}
		require.ElementsMatch(ompPropertyT{ht}, expected, got[0])
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
func TestPlanTransitionSafety(t *testing.T) { runPlanNode(t, planNode(t, "plan-safety.mjs")) }
func TestPlanOwnershipMutationProperty(t *testing.T) {
	// Stored teammate handles and explicit environment attribution use the
	// portable validator domain; the pure classification property keeps raw text.
	exchange := planPropertyFixture(t, "plan-safety.mjs", "stream")
	hegel.Test(t, func(ht *hegel.T) {
		owned, delegate, race := hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans())
		op := hegel.Draw(ht, hegel.SampledFrom([]string{"start", "block", "unblock", "done", "drop", "rm", "demote"}))
		lead := fmt.Sprintf("lead-%x", sha256.Sum256([]byte(hegel.Draw(ht, hegel.Text()))))[:29]
		if hegel.Draw(ht, hegel.Booleans()) {
			lead = ""
		}
		handle := fmt.Sprintf("worker-%x", sha256.Sum256([]byte(hegel.Draw(ht, hegel.Text()))))[:31]
		if hegel.Draw(ht, hegel.Booleans()) {
			handle = hegel.Draw(ht, hegel.SampledFrom([]string{"", "omp", "omp.model", lead, "other.worker"}))
		}
		note := hegel.Draw(ht, hegel.Text())
		input := map[string]any{"owned": owned, "delegate": delegate, "race": race, "op": op, "lead": lead, "teammate": handle, "note": note}
		var got struct {
			Writes    int            `json:"writes"`
			Error     bool           `json:"error"`
			ErrorText string         `json:"errorText"`
			Status    string         `json:"status"`
			Todo      string         `json:"todo"`
			Metadata  map[string]any `json:"metadata"`
			Owner     string         `json:"owner"`
		}
		exchange(ht, input, &got)
		allowed := owned && !delegate && (handle == lead || lead == "" && (handle == "omp" || strings.HasPrefix(handle, "omp.")))
		// Independent operation contract: compare the entire metadata snapshot,
		// including an arbitrary unrelated value, rather than accepting any write.
		initialTodo := "in_progress"
		switch op {
		case "start":
			initialTodo = "pending"
		case "unblock":
			initialTodo = "blocked"
		}
		wantMeta := map[string]any{"todo.phase": "Tasks", "todo.status": initialTodo, "teammate": handle, "example.note": note}
		if delegate {
			wantMeta["omp.subagent_id"] = "worker"
		}
		wantStatus, wantOwner := "open", "actor"
		if !owned {
			wantOwner = "other-actor"
		}
		if !allowed {
			require.Zero(ompPropertyT{ht}, got.Writes)
		} else if race && (op == "start" || op == "block" || op == "unblock" || op == "demote") {
			require.True(ompPropertyT{ht}, got.Error)
			require.Contains(ompPropertyT{ht}, got.ErrorText, "revision conflict")
			wantOwner = "other-actor"
			// Attention may have committed before the concurrent handoff. The
			// rejected conditional status write must retain the original todo.
			switch op {
			case "start", "unblock":
				wantMeta["work.attention"] = "ok"
			case "block":
				wantMeta["work.attention"] = "stuck"
				wantMeta["work.attention_msg"] = "awaiting review"
			}
		} else {
			require.False(ompPropertyT{ht}, got.Error)
			require.Positive(ompPropertyT{ht}, got.Writes)
			switch op {
			case "start", "unblock":
				wantMeta["todo.status"] = "in_progress"
				wantMeta["work.attention"] = "ok"
			case "block":
				wantMeta["todo.status"] = "blocked"
				wantMeta["work.attention"] = "stuck"
				wantMeta["work.attention_msg"] = "awaiting review"
			case "demote":
				wantMeta["todo.status"] = "pending"
			case "done":
				wantStatus, wantMeta["todo.status"] = "closed", "completed"
			case "drop", "rm":
				wantStatus, wantMeta["todo.status"] = "closed", "abandoned"
				if op == "rm" {
					wantMeta["todo.removed"] = true
				}
			}
		}
		require.Equal(ompPropertyT{ht}, wantStatus, got.Status)
		require.Equal(ompPropertyT{ht}, wantMeta["todo.status"], got.Todo)
		require.Equal(ompPropertyT{ht}, wantMeta, got.Metadata)
		require.Equal(ompPropertyT{ht}, wantOwner, got.Owner)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
func TestPlanEffectiveActorAuthenticatedDaemon(t *testing.T) {
	env, dir, pid := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{PlaintextToken: "plan-token", Actor: "daemon-actor", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{ProjectID: pid, Title: "Plan parent", Author: "daemon-actor"})
	require.NoError(t, err)
	cmd := planNode(t, "plan-auth.mjs", planTestBinary(t), parent.ShortID, dir)
	cmd.Env = append(cmd.Env, "KATA_HOME="+env.Home, "KATA_SERVER="+env.URL, "KATA_AUTH_TOKEN=plan-token", "KATA_AUTHOR=local-actor")
	runPlanNode(t, cmd)
}

func TestPlanEffectiveActorFederatedClaim(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	project, err := hub.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = hub.DB.EnableProjectFederation(ctx, project.ID, "hub-actor")
	require.NoError(t, err)
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Federated plan task", Author: "hub-actor"})
	require.NoError(t, err)
	spoke := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	replica, err := spoke.DB.CreateProjectWithUID(ctx, project.Name, project.UID)
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: replica.ID, Role: db.FederationRoleSpoke,
		HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID,
		ReplayHorizonEventID: 1, Actor: "daemon-actor", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	events, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, IssueUID: issue.UID, Types: []string{"issue.created"}, Limit: 1})
	require.NoError(t, err)
	require.Len(t, events, 1)
	event := events[0]
	inserted, err := spoke.DB.InsertRemoteEvent(ctx, replica.ID, db.RemoteEvent{
		EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter, ContentHash: event.ContentHash,
		Payload: jsontext.Value(event.Payload), CreatedAt: event.CreatedAt,
	})
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, spoke.DB.MaterializeFederatedProject(ctx, replica.ID))
	enrollment, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{Token: "plan-federation-token", SpokeInstanceUID: spoke.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "claim", Actor: "hub-actor"})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(replica.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: enrollment.Token, Capabilities: "claim"}))
	_, _, err = spoke.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "spoke-plan-token", Actor: "daemon-actor", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("version = 1\n[project]\nname = \"spoke-project\"\n"), 0600))
	cmd := planNode(t, "plan-auth.mjs", planTestBinary(t), project.Name+"#"+issue.ShortID, dir, "claim")
	cmd.Env = append(cmd.Env, "KATA_HOME="+spoke.Home, "KATA_SERVER="+spoke.URL, "KATA_AUTH_TOKEN=spoke-plan-token", "KATA_AUTHOR=local-actor")
	runPlanNode(t, cmd)
}

func TestPlanReplayRestorationOwnership(t *testing.T) { runPlanNode(t, planNode(t, "plan-replay.mjs")) }
