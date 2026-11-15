package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/fileevidence"
)

func checkFileCloseEvidence(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f, err := createIssueFixture(ctx, store, "example-project", "File proof", "operator", nil)
	require.NoError(t, err)
	// A local storage caller must not produce close events that replay rejects.
	_, badEvents, badChanged, badErr := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "operator", Evidence: []db.Evidence{{Type: "file", File: &fileevidence.Reference{Source: "notes", Path: "x"}}}})
	require.Error(t, badErr)
	require.Empty(t, badEvents)
	require.False(t, badChanged)
	afterBad, err := store.IssueByID(ctx, f.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, "open", afterBad.Status)
	size := int64(7)
	ref := &fileevidence.Reference{Source: "notes", Path: "results/report.md", Origin: store.InstanceUID(), SHA256: strings.Repeat("a", 64), Size: &size, Revision: "v1"}
	_, events, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "operator", Evidence: []db.Evidence{{Type: "test", Command: "go test ./example"}, {Type: "file", File: ref}}})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, events)
	saved, err := store.EventsByUIDs(ctx, f.Project.ID, []string{events[0].UID})
	require.NoError(t, err)
	require.Equal(t, events[0].Payload, saved[0].Payload)
	var payload struct {
		Evidence []db.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(saved[0].Payload), &payload))
	require.Len(t, payload.Evidence, 2)
	wantRef, _ := json.Marshal(ref)
	gotRef, _ := json.Marshal(payload.Evidence[1].File)
	require.JSONEq(t, string(wantRef), string(gotRef))
	found := false
	for event, err := range store.ExportEvents(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		if event.UID == events[0].UID {
			found = true
			require.JSONEq(t, events[0].Payload, string(event.Payload))
		}
	}
	require.True(t, found)
	// Ingest and materialize opaque references without configuring or reading any source.
	p, err := store.CreateProject(ctx, "replica-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: p.UID, Enabled: true})
	require.NoError(t, err)
	issueUID := "01CCCCCCCCCCCCCCCCCCCCCCCC"
	origin := "01DDDDDDDDDDDDDDDDDDDDDDDD"
	snapshot := newRemoteEvent(t, p, &issueUID, "issue.snapshot", "operator", origin, 1, IssueDocument(t, issueUID, jsontext.Value(`{"uid":"`+issueUID+`","short_id":"cccc","title":"File proof","author":"operator","status":"open","created_at":"2026-10-07T11:00:00Z"}`)))
	closed := newRemoteEvent(t, p, &issueUID, "issue.closed", "operator", origin, 2, jsontext.Value(events[0].Payload))
	for _, e := range []db.RemoteEvent{snapshot, closed} {
		_, err = store.InsertRemoteEvent(ctx, p.ID, e)
		require.NoError(t, err)
	}
	require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
	received, err := store.EventsByUIDs(ctx, p.ID, []string{closed.EventUID})
	require.NoError(t, err)
	require.JSONEq(t, string(closed.Payload), received[0].Payload)
	return nil
}
