package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Missing a stamp, hashing before stamping, or deriving only in mutation
// responses must fail this shared storage contract.
func checkEventTeammateStamped(t *testing.T, store db.Storage) error {
	ctx := db.WithEventTeammate(t.Context(), "claude")
	fixture, err := createIssueFixture(ctx, store, "example-project", "work", "alice", nil)
	require.NoError(t, err)
	peer, err := createFixtureIssue(ctx, store, fixture.Project.ID, "peer", "alice", nil)
	require.NoError(t, err)
	issueID := fixture.Issue.ID
	owner, title, priority := "bob", "edited", int64(2)
	steps := []struct {
		name, eventType string
		run             func() error
	}{
		{"close", "issue.closed", func() error {
			_, _, _, err := store.CloseIssue(ctx, issueID, "done", "alice", "Completed the requested work with supporting evidence.", nil)
			return err
		}},
		{"reopen", "issue.reopened", func() error {
			_, _, _, err := store.ReopenIssue(ctx, issueID, "alice")
			return err
		}},
		{"label", "issue.labeled", func() error {
			_, _, err := store.AddLabelAndEvent(ctx, issueID, db.LabelEventParams{EventType: "issue.labeled", Label: "bug", Actor: "alice"})
			return err
		}},
		{"unlabel", "issue.unlabeled", func() error {
			_, err := store.RemoveLabelAndEvent(ctx, issueID, db.LabelEventParams{EventType: "issue.unlabeled", Label: "bug", Actor: "alice"})
			return err
		}},
		{"metadata", "issue.metadata_updated", func() error {
			_, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issueID, Actor: "alice", Patch: map[string]jsontext.Value{"example": []byte(`true`)}})
			return err
		}},
		{"assign", "issue.assigned", func() error {
			_, _, _, err := store.UpdateOwner(ctx, issueID, &owner, "alice")
			return err
		}},
		{"claim owner", "issue.assigned", func() error {
			_, err := store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issueID, Actor: "alice", Force: true})
			return err
		}},
		{"lease claim", "claim.acquired", func() error {
			_, err := store.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: fixture.Project.ID, IssueRef: fixture.Issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: store.InstanceUID(), Holder: "alice", ClientKind: "agent"}, ClaimKind: "hard", Now: time.Now().UTC()})
			return err
		}},
		{"edit", "issue.updated", func() error {
			_, _, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: issueID, Title: &title, Actor: "alice"})
			return err
		}},
		{"priority", "issue.priority_set", func() error {
			_, _, _, err := store.UpdatePriority(ctx, issueID, &priority, "alice")
			return err
		}},
		{"link", "issue.linked", func() error {
			_, _, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issueID, ToIssueID: peer.ID, Type: "blocks", Author: "alice"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issueID, FromShortID: fixture.Issue.ShortID, FromUID: fixture.Issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "alice"})
			return err
		}},
		{"schedule", "issue.metadata_updated", func() error {
			_, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issueID, Actor: "alice", Patch: map[string]jsontext.Value{"scheduled_on": []byte(`"2030-01-01"`)}})
			return err
		}},
	}
	var closedEventUID string
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.NoError(t, step.run())
			events, err := store.EventsAfter(ctx, db.EventsAfterParams{AfterID: before, ProjectID: fixture.Project.ID, Limit: 100})
			require.NoError(t, err)
			require.NotEmpty(t, events)
			found := false
			for _, event := range events {
				found = found || event.Type == step.eventType
				if event.Type == "issue.closed" {
					closedEventUID = event.UID
				}
				assertStoredEventTeammate(t, event, "claude")
				assert.Equal(t, remoteEventHash(t, remoteEventFromStored(event)), event.ContentHash, "hash must cover the stored stamped payload")
			}
			assert.True(t, found, "missing %s", step.eventType)
		})
	}
	t.Run("window read derives teammate", func(t *testing.T) {
		require.NotEmpty(t, closedEventUID)
		events, err := store.EventsInWindow(ctx, db.EventsInWindowParams{
			Since:     "1970-01-01T00:00:00.000Z",
			Until:     "2999-01-01T00:00:00.000Z",
			ProjectID: fixture.Project.ID,
		})
		require.NoError(t, err)
		var found bool
		for _, event := range events {
			if event.UID == closedEventUID {
				assertStoredEventTeammate(t, event, "claude")
				found = true
			}
		}
		assert.True(t, found, "window read omitted the stamped close event")
	})

	t.Run("system", func(t *testing.T) {
		_, event, changed, err := store.UpdatePriority(ctx, issueID, nil, "system")
		require.NoError(t, err)
		require.True(t, changed)
		require.NotNil(t, event)
		assertStoredEventTeammate(t, *event, "")
		assert.Equal(t, remoteEventHash(t, remoteEventFromStored(*event)), event.ContentHash)
	})
	for _, replay := range []struct{ name, eventType, payload, teammate string }{
		{"unstamped", "issue.updated", `{"title":"remote"}`, ""},
		{"existing", "issue.updated", `{"teammate":"remote-agent","title":"remote"}`, "remote-agent"},
		{"creation metadata", "issue.created", `{"metadata":{"teammate":"record-agent"}}`, "record-agent"},
	} {
		t.Run("remote/"+replay.name, func(t *testing.T) {
			eventUID, err := uid.New()
			require.NoError(t, err)
			origin, err := uid.New()
			require.NoError(t, err)
			remote := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: fixture.Project.UID, ProjectName: fixture.Project.Name, IssueUID: &fixture.Issue.UID, Type: replay.eventType, Actor: "alice", HLCPhysicalMS: 1, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Payload: jsontext.Value(replay.payload)}
			remote.ContentHash = remoteEventHash(t, remote)
			inserted, err := store.InsertRemoteEvent(ctx, fixture.Project.ID, remote)
			require.NoError(t, err)
			require.True(t, inserted)
			events, err := store.EventsByUIDs(ctx, fixture.Project.ID, []string{eventUID})
			require.NoError(t, err)
			require.Len(t, events, 1)
			assert.Equal(t, replay.payload, events[0].Payload)
			assert.Equal(t, remote.ContentHash, events[0].ContentHash)
			assertStoredEventTeammate(t, events[0], replay.teammate)
			inserted, err = store.InsertRemoteEvent(ctx, fixture.Project.ID, remote)
			require.NoError(t, err)
			assert.False(t, inserted, "replay remains idempotent")
		})
	}
	return nil
}

func assertStoredEventTeammate(t *testing.T, event db.Event, want string) {
	t.Helper()
	assert.Equal(t, want, event.Teammate, "derived Event.Teammate on %s", event.Type)
	var payload struct {
		Teammate string `json:"teammate"`
	}
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	if event.Type != "issue.created" {
		assert.Equal(t, want, payload.Teammate, "payload attribution on %s", event.Type)
	}
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	var wire struct {
		Teammate string `json:"teammate"`
	}
	require.NoError(t, json.Unmarshal(encoded, &wire))
	assert.Equal(t, want, wire.Teammate, "derived event attribution on %s", event.Type)
}
