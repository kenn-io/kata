package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type retainedMigrationCase struct {
	projectID int64
	event     db.Event
	lastID    int64
	direction db.FederationQuarantineDirection
	retry     bool
}

func SeedRetainedHistoryMigration(t *testing.T, store db.Storage) func(db.Storage) {
	t.Helper()
	ctx := t.Context()
	var cases []retainedMigrationCase
	for _, tc := range []struct {
		name, actor, failure string
		disabled             bool
		direction            db.FederationQuarantineDirection
		retry                bool
	}{
		{"old-actor", "old-actor", "actor", false, db.FederationQuarantineDirectionPush, true},
		{"disabled", "old-actor", "actor", true, db.FederationQuarantineDirectionPush, true},
		{"same-actor", "new-actor", "actor", false, db.FederationQuarantineDirectionPush, false},
		{"other-error", "old-actor", "other", false, db.FederationQuarantineDirectionPush, false},
		{"pull-error", "old-actor", "actor", false, db.FederationQuarantineDirectionPull, false},
	} {
		p, err := store.CreateProject(ctx, tc.name)
		require.NoError(t, err)
		_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "task", Author: tc.actor})
		require.NoError(t, err)
		_, last, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "later task", Author: "new-actor"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: p.UID, Actor: "new-actor", PushEnabled: !tc.disabled, Enabled: !tc.disabled})
		require.NoError(t, err)
		failure := "unrelated validation failure"
		if tc.failure == "actor" {
			failure = fmt.Sprintf(`hub returned 400: {"error":{"message":"federation ingest validation: event %s actor \"%s\" does not match bound actor"}}`, event.UID, event.Actor)
		}
		_, err = store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{ProjectID: p.ID, Direction: tc.direction, FirstEventID: event.ID, LastEventID: event.ID, EventUIDs: []string{event.UID}, Error: failure})
		require.NoError(t, err)
		cases = append(cases, retainedMigrationCase{p.ID, event, last.ID, tc.direction, tc.retry})
	}
	return func(upgraded db.Storage) {
		for _, tc := range cases {
			binding, err := upgraded.FederationBindingByProject(ctx, tc.projectID)
			require.NoError(t, err)
			require.Zero(t, binding.PushCursorEventID)
			q, err := upgraded.ActiveFederationQuarantine(ctx, tc.projectID, tc.direction)
			if tc.retry {
				require.ErrorIs(t, err, db.ErrNotFound)
				require.Equal(t, tc.lastID, binding.RetainedThroughEventID)
			} else {
				require.NoError(t, err)
				require.NotZero(t, q.ID)
				require.Zero(t, binding.RetainedThroughEventID)
			}
			events, err := upgraded.EventsByUIDs(ctx, tc.projectID, []string{tc.event.UID})
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, tc.event.Actor, events[0].Actor)
			require.Equal(t, tc.event.ContentHash, events[0].ContentHash)
		}
	}
}
