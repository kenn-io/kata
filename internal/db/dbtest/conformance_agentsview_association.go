package dbtest

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/db"
)

// Exercises the same real producer on SQLite and PostgreSQL, never a fabricated
// issue.closed event. A stale selection must leave the issue open.
func checkAgentsViewAssociation(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "association-project")
	if err != nil {
		return err
	}
	i, err := createFixtureIssue(ctx, store, p.ID, "Completed associated work", "worker", nil)
	if err != nil {
		return err
	}
	_, err = store.EnableProjectFederation(ctx, p.ID, "worker")
	if err != nil {
		return err
	}
	// Missing enrolled root pin is a refusal even for a locally enabled hub.
	a := agentsviewrelay.Association{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: store.InstanceUID(), ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "claude", NativeSessionID: "00000000-0000-4000-8000-000000000001", SessionID: "00000000-0000-4000-8000-000000000001", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 12, LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: store.InstanceUID(), ProjectUID: p.UID}}}
	_, _, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: i.ID, Reason: "done", Actor: "worker", AgentsViewAssociation: &a})
	require.ErrorIs(t, err, db.ErrAgentsViewRouteChanged)
	require.False(t, changed)
	current, err := store.IssueByID(ctx, i.ID)
	if err != nil {
		return err
	}
	require.Equal(t, "open", current.Status)
	// A normal close still works and does not create a phantom association.
	_, events, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: i.ID, Reason: "done", Actor: "worker"})
	if err != nil {
		return err
	}
	require.True(t, changed)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &body))
	require.NotContains(t, body, "agentsview_association")
	// The successful path uses an existing enrolled pin and loaded fixture key.
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	pin := db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), PublicKey: key.Public().(ed25519.PublicKey)}
	if err := store.PinRootAuthority(ctx, pin); err != nil {
		return err
	}
	signed := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}, "worker")
	second, _, err := store.CreateIssue(signed, db.CreateIssueParams{ProjectID: p.ID, Title: "Consented completed work", Author: "worker"})
	if err != nil {
		return err
	}
	a.IssueUID = second.UID
	a.RootKeyID = pin.KeyID
	_, events, changed, err = store.CloseIssueGuarded(signed, db.CloseIssueParams{IssueID: second.ID, ExpectedProjectID: p.ID, Reason: "done", Actor: "worker", AgentsViewAssociation: &a})
	if err != nil {
		return err
	}
	require.True(t, changed)
	require.NotEmpty(t, events)
	var accepted struct {
		Association *agentsviewrelay.Association `json:"agentsview_association"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &accepted))
	require.Equal(t, &a, accepted.Association)
	_, again, changed, err := store.CloseIssueGuarded(signed, db.CloseIssueParams{IssueID: second.ID, Reason: "done", Actor: "worker", AgentsViewAssociation: &a})
	if err != nil {
		return err
	}
	require.False(t, changed)
	require.Empty(t, again)

	return nil
}
