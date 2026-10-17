package federation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

func TestSyncFederationMoveHistoryPublishesOnce(t *testing.T) {
	for _, scenario := range []string{"source-first", "destination-first", "failed-destination-pull", "failed-source-overlap", "readonly-chain-overlap"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "publication-source", "publication-middle", "publication-target")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			if scenario == "readonly-chain-overlap" {
				for _, project := range spoke.projects {
					binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
					require.NoError(t, err)
					binding.PushEnabled = false
					_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
					require.NoError(t, err)
				}
			}
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "initial title", Author: "worker"})
			require.NoError(t, err)
			_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "source discussion"})
			require.NoError(t, err)
			title := "edited source title"
			_, _, _, err = hub.DB.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "worker"})
			require.NoError(t, err)
			counts := map[string]int{}
			sync := func(index int) error {
				binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[index].ID)
				if err != nil {
					return err
				}
				return SyncFederationOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[index].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
					for _, event := range events {
						if event.Type != db.ProjectScopeResetEventType {
							counts[event.UID]++
						}
					}
				}, spoke.credentials)
			}
			if scenario != "destination-first" && scenario != "failed-source-overlap" && scenario != "readonly-chain-overlap" {
				require.NoError(t, sync(0))
			}
			issue, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			if scenario == "failed-destination-pull" {
				t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
				require.Error(t, sync(1))
				t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
			}
			require.NoError(t, sync(1))
			if scenario == "failed-source-overlap" {
				t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
				require.Error(t, sync(0))
				t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
			}
			if scenario != "readonly-chain-overlap" {
				require.NoError(t, sync(0), "overlapping source poll must not republish destination-backfilled history")
			}
			issue, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[1].ID, ToProjectID: projects[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			if scenario == "readonly-chain-overlap" {
				require.NoError(t, sync(1), "the former destination applies the next move before its destination polls")
			}
			require.NoError(t, sync(2))
			require.NoError(t, sync(1))
			require.NoError(t, sync(2))
			events, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
			require.NoError(t, err)
			require.NotEmpty(t, events)
			for _, event := range events {
				require.Equal(t, 1, counts[event.UID], "%s must publish once across moves and overlapping streams", event.Type)
			}
		})
	}
}

func TestSyncFederationMovedIssueRecoversRetainedEditPublication(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "edit-recovery-source", "edit-recovery-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "original title", Author: "worker"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, spoke.sync(t, i))
	}
	title := "recovered title"
	_, edit, _, err := hub.DB.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "worker"})
	require.NoError(t, err)
	if edit == nil {
		t.Fatal("missing edit")
	}
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[1].ID)
	require.NoError(t, err)
	// Stored pull status has millisecond precision. Equal prior start/success
	// and a backwards wall clock must not hide this turn's unfinished apply.
	prior := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	require.NoError(t, spoke.env.DB.RecordFederationSyncPullStarted(ctx, binding.ProjectID, prior))
	require.NoError(t, spoke.env.DB.RecordFederationSyncPullSuccess(ctx, binding.ProjectID, prior))
	var delivered []string
	sync := func() error {
		return SyncFederationOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[1].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
			for _, event := range events {
				delivered = append(delivered, event.UID)
			}
		}, spoke.credentials)
	}
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
	require.Error(t, sync())
	require.Empty(t, delivered)
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	require.NoError(t, sync())
	require.Contains(t, delivered, edit.UID)
	got, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, title, got.Title)
}

func TestSyncFederationMovedIssueRecoversAcrossProjectStreams(t *testing.T) {
	for _, scenario := range []string{"failed-source", "failed-source-future-status", "active-source", "active-source-stale-error"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "cross-recovery-source", "cross-recovery-target")
			receiver := newMoveDeliverySpoke(t, hub, projects)
			sender := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "original title", Author: "worker"})
			require.NoError(t, err)
			for _, spoke := range []moveDeliverySpoke{receiver, sender} {
				for i := range projects {
					require.NoError(t, spoke.sync(t, i))
				}
			}
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NoError(t, receiver.sync(t, 1))
			old, err := sender.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			title := "recovered delayed title"
			_, edit, _, err := sender.env.DB.EditIssue(ctx, db.EditIssueParams{IssueID: old.ID, Title: &title, Actor: "worker"})
			require.NoError(t, err)
			if edit == nil {
				t.Fatal("missing delayed source edit")
			}
			require.NoError(t, sender.sync(t, 0))
			var delivered []db.Event
			var sourceBeforeApply func()
			sync := func(index int) error {
				binding, err := receiver.env.DB.FederationBindingByProject(ctx, receiver.projects[index].ID)
				if err != nil {
					return err
				}
				var store db.Storage = receiver.env.DB
				if index == 0 {
					store = &beforePullApplyStore{Storage: store, before: sourceBeforeApply}
				}
				return SyncFederationOnceWithPulledEvents(ctx, store, binding, receiver.credentials[projects[index].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
					delivered = append(delivered, events...)
				}, receiver.credentials)
			}
			if scenario == "failed-source-future-status" {
				prior := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
				require.NoError(t, receiver.env.DB.RecordFederationSyncPullStarted(ctx, receiver.projects[0].ID, prior))
				require.NoError(t, receiver.env.DB.RecordFederationSyncPullSuccess(ctx, receiver.projects[0].ID, prior))
			}
			if scenario != "failed-source" && scenario != "failed-source-future-status" {
				if scenario == "active-source-stale-error" {
					require.NoError(t, receiver.env.DB.RecordFederationSyncError(ctx, receiver.projects[0].ID, errors.New("previous attempt failed"), time.Now().UTC().Add(-time.Hour)))
				}
				sourceBeforeApply = func() {
					before, err := receiver.env.DB.MaxEventID(ctx)
					require.NoError(t, err)
					require.NoError(t, sync(1), "destination can overlap a healthy source pull before its apply")
					current, err := receiver.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Equal(t, title, current.Title)
					var reset bool
					for _, event := range delivered {
						reset = reset || (event.Type == db.ProjectScopeResetEventType && event.ProjectID == receiver.projects[1].ID && event.ID > before)
					}
					require.True(t, reset, "destination apply must invalidate its view even while source owns hook delivery")
					for _, event := range delivered {
						require.NotEqual(t, edit.UID, event.UID, "an active source pull has not failed and owns publication")
					}
				}
				require.NoError(t, sync(0))
				count := 0
				for _, event := range delivered {
					if event.UID == edit.UID {
						count++
					}
				}
				require.Equal(t, 1, count)
				return
			}
			t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
			require.Error(t, sync(0))
			require.Empty(t, delivered)
			t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
			before, err := receiver.env.DB.MaxEventID(ctx)
			require.NoError(t, err)
			require.NoError(t, sync(1), "normal destination polling must recover retained source work")
			got, err := receiver.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, title, got.Title)
			var foundEdit, foundReset bool
			for _, event := range delivered {
				foundEdit = foundEdit || event.UID == edit.UID
				foundReset = foundReset || (event.Type == db.ProjectScopeResetEventType && event.ProjectID == receiver.projects[1].ID && event.ID > before)
			}
			require.True(t, foundEdit, "an unfinished source pull's retained edit must publish through the destination")
			require.True(t, foundReset, "recovered cross-stream work must wake the current project")
			_, err = receiver.env.DB.LeaveFederationReplica(ctx, receiver.projects[0].ID)
			require.NoError(t, err, "recovery must not require another source turn before source leave")
		})
	}
}

type beforePullApplyStore struct {
	db.Storage
	before func()
}

func (s *beforePullApplyStore) MaterializeFederatedProject(ctx context.Context, projectID int64) error {
	if s.before != nil {
		s.before()
	}
	return s.Storage.MaterializeFederatedProject(ctx, projectID)
}
