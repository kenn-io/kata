package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

func checkTrackerControlReceipts(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-control-project")
	require.NoError(t, err)
	control := tracker.ControlReceipt{Version: 1, RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, UID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Principal: tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}, Selection: tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Kind: tracker.ControlBind, SourceKey: "notion:source", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: tracker.ControlPending}
	receipts := store.(db.TrackerControlStore)
	admitted, fresh, err := receipts.BeginTrackerControl(ctx, control)
	require.NoError(t, err)
	require.True(t, fresh)
	require.Equal(t, control, admitted)
	admitted, fresh, err = receipts.BeginTrackerControl(ctx, control)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Equal(t, control, admitted)
	changed := control
	changed.RequestDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, _, err = receipts.BeginTrackerControl(ctx, changed)
	require.ErrorIs(t, err, tracker.ErrControlConflict)
	done := control
	done.State = tracker.ControlComplete
	done.Result = &tracker.ControlResult{Code: "ok", Enabled: true}
	require.NoError(t, receipts.CompleteTrackerControl(ctx, control, done))
	existing, fresh, err := receipts.BeginTrackerControl(ctx, control)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Equal(t, done, existing)
	_, _, err = receipts.BeginTrackerControl(db.WithAuthorizedProjects(ctx, []string{}), control)
	require.Error(t, err)
	return nil
}

func checkTrackerControlCapacity(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-capacity-project")
	require.NoError(t, err)
	base := tracker.ControlReceipt{Version: 1, RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}, Selection: tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Kind: tracker.ControlRun, SourceKey: "notion:source", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: tracker.ControlPending}
	receipts := store.(db.TrackerControlStore)
	var first tracker.ControlReceipt
	for i := range 64 {
		p := base
		p.UID, err = uid.New()
		require.NoError(t, err)
		_, fresh, err := receipts.BeginTrackerControl(ctx, p)
		require.NoError(t, err)
		require.True(t, fresh)
		if i == 0 {
			first = p
		}
	}
	overflow := base
	overflow.UID, err = uid.New()
	require.NoError(t, err)
	_, _, err = receipts.BeginTrackerControl(ctx, overflow)
	require.ErrorIs(t, err, tracker.ErrControlCapacity)
	done := first
	done.State = tracker.ControlComplete
	done.Result = &tracker.ControlResult{Code: "ok"}
	require.NoError(t, receipts.CompleteTrackerControl(ctx, first, done))
	_, fresh, err := receipts.BeginTrackerControl(ctx, overflow)
	require.NoError(t, err)
	require.True(t, fresh)
	retained, err := receipts.ReadTrackerControl(ctx, project.UID, first.UID)
	require.NoError(t, err)
	require.Equal(t, done, retained, "capacity must not evict replay evidence")
	return nil
}
