package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type agentsViewFixturePort struct {
	selected       agentsViewSelection
	result         agentsViewCloseResult
	original       agentsViewCloseEvent
	closes, shares int
	uploaded       []byte
	failShare      error
}

func (p *agentsViewFixturePort) Select(context.Context, string) (agentsViewSelection, error) {
	return p.selected, nil
}
func (p *agentsViewFixturePort) Close(_ context.Context, _ string, _ map[string]any, _ map[string]string) (agentsViewCloseResult, error) {
	p.closes++
	return p.result, nil
}
func (p *agentsViewFixturePort) Original(context.Context, string, string) (agentsViewCloseEvent, error) {
	return p.original, nil
}
func (p *agentsViewFixturePort) Share(_ context.Context, _ string, _ agentsViewConsent, _ string, r io.Reader) error {
	p.shares++
	p.uploaded, _ = io.ReadAll(r)
	return p.failShare
}

func agentsViewWorkflowFixture(t *testing.T) (*agentsViewWorkflow, *agentsViewFixturePort, agentsViewConsent, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":1,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`), 0600))
	port := &agentsViewFixturePort{selected: agentsViewSelection{
		InstanceUID: "daemon-a", IssueUID: "issue-a", ProjectUID: "project-a", AuthorityUID: "root-a", ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), LocatorBase: "https://archive.example",
		Route:     []agentsViewRoute{{InstanceUID: "daemon-a", ProjectUID: "project-a", BindingUID: "binding-a", UpstreamInstanceUID: "root-a", ResetEpoch: 1}},
		Discovery: []byte(`{"fixture":"verified by port"}`), ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}}
	workflow := &agentsViewWorkflow{port: port}
	consent, err := workflow.Prepare(t.Context(), "example#abcd", path, agentsViewNativeIdentity{Agent: "pi", NativeSessionID: "selected"})
	require.NoError(t, err)
	return workflow, port, consent, path
}

// Contract: only a confirmed newly committed close can automatically upload.
func TestAgentsViewCloseRecoveryStates(t *testing.T) {
	for _, state := range []string{"new", "noop", "replay", "uncertain", "dry", "wrong-event"} {
		t.Run(state, func(t *testing.T) {
			workflow, port, consent, path := agentsViewWorkflowFixture(t)
			event := agentsViewCloseEvent{UID: "close-a", Type: "issue.closed", IssueUID: "issue-a", ProjectUID: "project-a", Association: consent.Association}
			port.result = agentsViewCloseResult{Changed: true, Event: &event}
			switch state {
			case "noop":
				port.result.Changed = false
			case "replay":
				port.result.Reused = true
			case "uncertain":
				port.result.Event = nil
			case "wrong-event":
				event.IssueUID = "other-issue"
			}
			_, err := workflow.Close(t.Context(), "example#abcd", consent, path, consent.Association.SnapshotSHA256, agentsViewNativeIdentity{"pi", "selected"}, state == "dry", nil, nil)
			if state == "uncertain" || state == "wrong-event" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			wantShares := 0
			if state == "new" {
				wantShares = 1
			}
			require.Equal(t, wantShares, port.shares)
		})
	}
}

func TestAgentsViewManualShareUsesOriginalIdentityAndSameRoute(t *testing.T) {
	workflow, port, consent, path := agentsViewWorkflowFixture(t)
	port.original = agentsViewCloseEvent{UID: "close-a", Type: "issue.closed", IssueUID: "issue-a", ProjectUID: "project-a", Association: consent.Association}
	// Later terminal's discovered session is irrelevant to manual recovery.
	t.Setenv("PI_SESSION_ID", "another-session")
	require.NoError(t, workflow.Share(t.Context(), "example#abcd", consent, path, "close-a", consent.Association.SnapshotSHA256))
	require.Equal(t, 0, port.closes)
	require.Equal(t, 1, port.shares)
	port.selected.Route[0].ResetEpoch++
	require.Error(t, workflow.Share(t.Context(), "example#abcd", consent, path, "close-a", consent.Association.SnapshotSHA256))
	require.Equal(t, 1, port.shares)
}

func TestAgentsViewConsentRejectsRetargetAndChangedBytesBeforeMutation(t *testing.T) {
	for _, state := range []string{"digest", "file", "instance", "issue", "root", "service", "native", "body-identity", "root-key"} {
		t.Run(state, func(t *testing.T) {
			workflow, port, consent, path := agentsViewWorkflowFixture(t)
			digest := consent.Association.SnapshotSHA256
			native := agentsViewNativeIdentity{"pi", "selected"}
			switch state {
			case "digest":
				digest = strings.Repeat("b", 64)
			case "file":
				require.NoError(t, os.WriteFile(path, []byte("changed"), 0600))
			case "instance":
				port.selected.InstanceUID = "other-daemon"
			case "issue":
				port.selected.IssueUID = "other-issue"
			case "root":
				port.selected.AuthorityUID = "other-root"
			case "service":
				port.selected.ServiceID = strings.Repeat("b", 64)
			case "root-key":
				port.selected.RootKeyID = strings.Repeat("d", 64)
			case "native":
				native.NativeSessionID = "other-session"
			case "body-identity":
				consent.Association.SessionID = "pi:other-session"
			}
			_, err := workflow.Close(t.Context(), "example#abcd", consent, path, digest, native, false, nil, nil)
			require.Error(t, err)
			require.Zero(t, port.closes)
			require.Zero(t, port.shares)
		})
	}
}

func TestAgentsViewPrepareCapturesExactIdentityAndNoUpload(t *testing.T) {
	workflow, port, consent, path := agentsViewWorkflowFixture(t)
	require.Zero(t, port.closes)
	require.Zero(t, port.shares)
	_, err := workflow.Prepare(t.Context(), "example#abcd", path, agentsViewNativeIdentity{"pi", "wrong"})
	require.Error(t, err)
	raw, err := json.Marshal(consent)
	require.NoError(t, err)
	require.NotContains(t, string(raw), path)
	require.NotContains(t, string(raw), "selected transcript content")
}
