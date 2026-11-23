package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type retainedCountStore struct{ db.Storage }

func (s retainedCountStore) PendingFederationPushEvents(context.Context, int64, string, int64, int) ([]db.Event, error) {
	return nil, errors.New("status read the event stream")
}

func TestRetainedCountDoesNotReadEventPages(t *testing.T) {
	ctx := t.Context()
	store := openClaimGateHelperDB(t)
	project, err := store.CreateProject(ctx, "retained-count")
	require.NoError(t, err)
	_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old", Author: "old-actor"})
	require.NoError(t, err)
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	binding.PushCursorEventID = event.ID
	count, err := federationRetainedEventCount(ctx, retainedCountStore{store}, binding)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}
