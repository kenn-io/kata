package daemon_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
)

// A request that moves to a project the owner cannot see leaves the departure
// project's inbox. The owner is pinged, and the ping names nothing else.
func TestMCPEventsMoveIntoAHiddenProjectWakesTheDepartureProject(t *testing.T) {
	for name, selection := range map[string][]string{"dynamic": nil, "explicit": {"public"}} {
		t.Run(name, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newEventsFixture(t, store)
				ctx := t.Context()
				mover := notifyIssue(t, store, f.public.ID, "mover")
				var got pingRecorder
				service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
				args := mcpevents.Arguments{For: "example-user"}
				if selection != nil {
					args.ProjectUIDs = []string{f.public.UID}
				}
				subscribeEvents(t, service, f.outsider, args)
				require.NoError(t, service.Drain(ctx))

				_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: mover.ID, FromProjectID: f.public.ID, ToProjectID: f.hidden.ID, IfMatchRev: mover.Revision, Actor: "coordinator"})
				require.NoError(t, err)
				require.NoError(t, service.Drain(ctx))

				assert.Equal(t, int64(1), got.pings.Load())
				assert.Equal(t, []string{"example-user"}, got.recipients())
			})
		})
	}
}

// Activity in a project the owner cannot see never pings, even for a
// subscription that follows every project.
func TestMCPEventsHiddenProjectActivityNeverPings(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.outsider, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, service.Drain(ctx))

		notifyIssue(t, store, f.hidden.ID, "hidden request")
		require.NoError(t, service.Drain(ctx))
		assert.Zero(t, got.pings.Load())

		notifyIssue(t, store, f.public.ID, "public request")
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load())
	})
}

// The scan position is stored, so a change committed while no daemon runs is
// delivered after the next start, once.
func TestMCPEventsRestartDeliversChangesMadeWhileStopped(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		keyPath := filepath.Join(t.TempDir(), "mcp-events.key")
		var got pingRecorder
		before, _ := newDaemonServiceWithKeys(t, store, f, mcpevents.NewFileKeyProvider(keyPath), got.respond)
		subscribeEvents(t, before, f.member, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, before.Drain(ctx))

		notifyIssue(t, store, f.public.ID, "while stopped")
		after, _ := newDaemonServiceWithKeys(t, store, f, mcpevents.NewFileKeyProvider(keyPath), got.respond)
		require.NoError(t, after.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load())

		require.NoError(t, after.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load(), "a scanned change is not delivered again")
	})
}

// A request behind more unrelated events than one scan page is still found.
func TestMCPEventsScanReachesARequestBehindALongBacklog(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, service.Drain(ctx))

		for range 600 {
			_, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: f.publicIssue.ID, Author: "coordinator", Body: "progress note"})
			require.NoError(t, err)
		}
		notifyIssue(t, store, f.public.ID, "behind the backlog")
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load())
	})
}

// Archiving a project pings the owners who could see it, whether they
// follow every project or name it. Purging the archived project afterwards
// changes no inbox, so it pings nobody again.
func TestMCPEventsArchivingThenPurgingAProjectPingsOnce(t *testing.T) {
	for name, explicit := range map[string]bool{"dynamic": false, "explicit": true} {
		t.Run(name, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newEventsFixture(t, store)
				ctx := t.Context()
				doomed, err := store.CreateProject(ctx, "doomed-project")
				require.NoError(t, err)
				_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: doomed.ID, Title: "doomed", Author: "coordinator"})
				require.NoError(t, err)
				var got pingRecorder
				service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
				args := mcpevents.Arguments{For: "example-user"}
				if explicit {
					args.ProjectUIDs = []string{f.public.UID, doomed.UID}
					sortStrings(args.ProjectUIDs)
				}
				subscribeEvents(t, service, f.member, args)
				require.NoError(t, service.Drain(ctx))

				_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: doomed.ID, Actor: "admin", Force: true})
				require.NoError(t, err)
				require.NoError(t, service.Drain(ctx))
				assert.Equal(t, int64(1), got.pings.Load(), "archiving")

				_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: doomed.ID, Actor: "admin"})
				require.NoError(t, err)
				require.NoError(t, service.Drain(ctx))
				assert.Equal(t, int64(1), got.pings.Load(), "purging the archived project")
			})
		})
	}
}

func TestMCPEventsTeamMembershipChangePingsThatActor(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.outsider, mcpevents.Arguments{For: "outsider-user"})
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "member-user"})
		require.NoError(t, service.Drain(ctx))

		_, err := store.SetTeamMembership(ctx, f.teamUID, "outsider-actor", true, db.BootstrapActor)
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, []string{"outsider-user"}, got.recipients(), "joining exposes the team project's requests")

		_, err = store.SetTeamMembership(ctx, f.teamUID, "outsider-actor", false, db.BootstrapActor)
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, []string{"outsider-user", "outsider-user"}, got.recipients(), "leaving hides them again")
	})
}

// A subscription hears about changes after it starts. A request committed
// before the subscribe call, even one the worker has not scanned yet, is the
// client's initial inbox read, not a ping.
func TestMCPEventsNewSubscriptionIsNotPingedForEarlierChanges(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		notifyIssue(t, store, f.public.ID, "before the subscription")
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, service.Drain(ctx))
		assert.Zero(t, got.pings.Load())

		notifyIssue(t, store, f.public.ID, "after the subscription")
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load())
	})
}

// downAuthority reports authority unavailable for one principal while down.
type downAuthority struct {
	mcpevents.Authority
	subject string
	down    atomic.Bool
}

func (a *downAuthority) Reacquire(ctx context.Context, p mcpevents.Principal) (mcpevents.Authorized, error) {
	if a.down.Load() && p.Subject == a.subject {
		return nil, mcpevents.ErrAuthorityUnavailable
	}
	return a.Authority.Reacquire(ctx, p)
}

// One owner whose authority cannot be checked must not hold back everyone
// else. Its own ping waits until authority returns and is then sent once.
func TestMCPEventsOneOwnersOutageDoesNotStallOthers(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		authority := &downAuthority{Authority: f.authority, subject: f.outsider.Subject}
		f.authority = authority
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.outsider, mcpevents.Arguments{For: "example-user"})
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, service.Drain(ctx))

		authority.down.Store(true)
		notifyIssue(t, store, f.public.ID, "during the outage")
		head, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load(), "the healthy owner is pinged")
		position, _, err := store.MCPEventsScanPosition(ctx)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, position, head, "the shared scan moved past the event")

		authority.down.Store(false)
		service.Recheck()
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(2), got.pings.Load(), "the deferred owner is pinged once authority returns")
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(2), got.pings.Load(), "nobody is pinged twice")
	})
}

// A subscribe call catches the shared scan up before it activates. The
// caller's request may be limited to some projects, but the scan serves every
// subscriber, so it must read the whole archive: a request outside the
// caller's projects still reaches the subscriber that can see it.
func TestMCPEventsCatchUpIgnoresTheCallersProjectLimit(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user", ProjectUIDs: []string{f.hidden.UID}})
		require.NoError(t, service.Drain(ctx))

		notifyIssue(t, store, f.hidden.ID, "outside the caller's projects")
		limited := db.WithAuthorizedProjects(ctx, []string{f.public.UID})
		_, err := service.Subscribe(limited, f.outsider, mcpevents.ForwarderScope{All: true}, mcpevents.SubscribeRequest{
			Name: mcpevents.EventName, Arguments: mcpevents.Arguments{For: "outsider-user"},
			Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/events", Secret: "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		})
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, []string{"example-user"}, got.recipients())
	})
}

// An issue-scoped token is woken by lifecycle and link changes inside its
// project, since they can move requests in or out of its subtree. Activity in
// a project it cannot see never wakes it.
func TestMCPEventsScopedTokenIgnoresOtherProjects(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.scoped, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, service.Drain(ctx))

		_, _, _, err := store.SoftDeleteIssue(ctx, f.hiddenIssue.ID, "coordinator")
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Zero(t, got.pings.Load(), "a project the token cannot see")

		_, _, _, err = store.SoftDeleteIssue(ctx, f.childIssue.ID, "coordinator")
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load(), "its own project")
	})
}

// A change to which projects an owner can see pings that owner's
// subscriptions whether the owner gained or lost a project. Owners whose
// visible projects did not change are not pinged, even for a project that
// was archived, so they learn nothing about projects they cannot see.
func TestMCPEventsVisibleProjectChangesPingOnlyAffectedOwners(t *testing.T) {
	for _, change := range []string{"policy hides it", "archive"} {
		t.Run(change, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newEventsFixture(t, store)
				ctx := t.Context()
				var got pingRecorder
				service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
				subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "member-user"})
				subscribeEvents(t, service, f.outsider, mcpevents.Arguments{For: "outsider-user"})
				require.NoError(t, service.Drain(ctx))

				var err error
				if change == "archive" {
					_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: f.hidden.ID, Actor: db.BootstrapActor, Force: true})
				} else {
					other, _, terr := store.CreateTeam(ctx, "other-team", db.BootstrapActor)
					require.NoError(t, terr)
					_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: f.hidden.UID, Visibility: "teams", TeamUIDs: []string{other.UID}}, db.BootstrapActor)
				}
				require.NoError(t, err)
				require.NoError(t, service.Drain(ctx))
				assert.Equal(t, []string{"member-user"}, got.recipients(), "only the owner that lost the project")

				require.NoError(t, service.Drain(ctx))
				assert.Equal(t, []string{"member-user"}, got.recipients(), "once")
			})
		})
	}
}

// visibilityHookAuthority runs a hook once, just before the first visible
// project read, to commit a change between the scan's reads.
type visibilityHookAuthority struct {
	mcpevents.Authority
	hook func()
}

func (a *visibilityHookAuthority) Reacquire(ctx context.Context, p mcpevents.Principal) (mcpevents.Authorized, error) {
	auth, err := a.Authority.Reacquire(ctx, p)
	if err != nil {
		return nil, err
	}
	return visibilityHookAuthorized{Authorized: auth, owner: a}, nil
}

type visibilityHookAuthorized struct {
	mcpevents.Authorized
	owner *visibilityHookAuthority
}

func (a visibilityHookAuthorized) VisibleProjects(ctx context.Context, selection []string) ([]mcpevents.Project, error) {
	if hook := a.owner.hook; hook != nil {
		a.owner.hook = nil
		hook()
	}
	return a.Authorized.VisibleProjects(ctx, selection)
}

// An access grant that commits while the scan is recomputing visible
// projects, after it fixed the range it reads, is still delivered.
func TestMCPEventsAccessGrantDuringAFingerprintRefreshStillPings(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		authority := &visibilityHookAuthority{Authority: f.authority}
		f.authority = authority
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.outsider, mcpevents.Arguments{For: "outsider-user"})
		require.NoError(t, service.Drain(ctx))

		// Another actor's membership change makes the scan recompute visible
		// projects without changing this subscriber's, so the grant below is
		// the only reason to ping.
		_, err := store.SetTeamMembership(ctx, f.teamUID, "unrelated-actor", true, db.BootstrapActor)
		require.NoError(t, err)
		authority.hook = func() {
			_, _, err := store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: f.hidden.UID, Visibility: "all"}, db.BootstrapActor)
			require.NoError(t, err)
		}
		require.NoError(t, service.Drain(ctx))
		require.NoError(t, service.Drain(ctx))
		assert.Nil(t, authority.hook, "the grant committed during the scan")
		assert.Positive(t, got.pings.Load(), "the outsider gained the team project")
	})
}

// A project an owner could see becoming team-only pings that owner, even when
// it was the owner's only project and its catalog is now empty.
func TestMCPEventsLosingTheOnlyVisibleProjectPings(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.outsider, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, service.Drain(ctx))

		_, _, err := store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: f.public.UID, Visibility: "teams", TeamUIDs: []string{f.teamUID}}, db.BootstrapActor)
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load())
	})
}

// Merging a selected project into one the selection does not name removes it
// from the selection's view. The merge event belongs to the target, so the
// subscription is pinged through its visible-project fingerprint.
func TestMCPEventsMergingASelectedProjectAwayPings(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		source, err := store.CreateProject(ctx, "source-project")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "target-project")
		require.NoError(t, err)
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		selection := []string{f.public.UID, source.UID}
		sortStrings(selection)
		result := subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user", ProjectUIDs: selection})
		require.NoError(t, service.Drain(ctx))

		_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: db.BootstrapActor})
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load())
		row, err := store.GetMCPEventSubscription(ctx, result.ID)
		require.NoError(t, err)
		assert.Equal(t, db.MCPEventStateActive, row.State, "the other selected project keeps it active")
	})
}

// insertSnapshotEvent records an issue.snapshot the way federation ingest
// does, carrying a peer's whole issue state, for an issue the test owns.
func insertSnapshotEvent(t *testing.T, store db.Storage, project db.Project, issue db.Issue, uid, payload string) {
	t.Helper()
	execer, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	_, err := execer.ExecContext(t.Context(), `INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
		issue_id, issue_uid, type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
		VALUES($1, '01ARZ3NDEKTSV4RRFFQ69G5FAW', $2, $3, $4, $5, 'issue.snapshot', 'coordinator', $6, 1, 0, $7)`,
		uid, project.ID, project.Name, issue.ID, issue.UID, payload, strings.Repeat("a", 64))
	require.NoError(t, err)
}

// A federated snapshot can delete or restore an issue, which can move a
// scoped token's requests in or out of its subtree. A subscription filtered
// to a grandchild hears a snapshot of its parent, as it hears a local delete.
func TestMCPEventsSnapshotOfAParentWakesAScopedFilteredSubscription(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		grandchild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "grandchild", Author: "coordinator",
			Links: []db.InitialLink{{Type: "parent", ToNumber: f.childIssue.ID}}})
		require.NoError(t, err)
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.scoped, mcpevents.Arguments{For: "example-user", IssueUID: grandchild.UID})
		require.NoError(t, service.Drain(ctx))

		insertSnapshotEvent(t, store, f.public, f.childIssue, "01ARZ3NDEKTSV4RRFFQ69G5FB1", `{"deleted_at":"2026-11-04T00:00:00Z"}`)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load(), "a snapshot deleting the parent")

		insertSnapshotEvent(t, store, f.public, f.childIssue, "01ARZ3NDEKTSV4RRFFQ69G5FB2", `{"deleted_at":null}`)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(2), got.pings.Load(), "a snapshot restoring it")

		insertSnapshotEvent(t, store, f.hidden, f.hiddenIssue, "01ARZ3NDEKTSV4RRFFQ69G5FB3", `{"deleted_at":"2026-11-04T00:00:00Z"}`)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(2), got.pings.Load(), "not in a project the token cannot see")
	})
}

// insertRemoteIssueCreated records a pulled issue.created the way a spoke
// pull does before it materializes the issue: issue_id is NULL and the issue
// row does not exist yet. The payload asks example-user for work.
func insertRemoteIssueCreated(t *testing.T, store db.Storage, project db.Project, eventUID, issueUID string) int64 {
	t.Helper()
	execer, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	payload := `{"title":"remote","metadata":{"` + notification.MetadataKey("example-user") + `":{"from":"coordinator","message":"review"}}}`
	_, err := execer.ExecContext(t.Context(), `INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
		issue_uid, type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
		VALUES($1, '01ARZ3NDEKTSV4RRFFQ69G5FAW', $2, $3, $4, 'issue.created', 'coordinator', $5, 1, 0, $6)`,
		eventUID, project.ID, project.Name, issueUID, payload, strings.Repeat("a", 64))
	require.NoError(t, err)
	head, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	return head
}

// Moving an intermediate parent alone out of a scoped token's project, and
// back, moves its descendants out of and back into the token's subtree. A
// subscription filtered to a grandchild hears both moves.
func TestMCPEventsMovingAParentAloneWakesAScopedFilteredSubscription(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		grandchild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "grandchild", Author: "coordinator",
			Links: []db.InitialLink{{Type: "parent", ToNumber: f.childIssue.ID}}})
		require.NoError(t, err)
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.scoped, mcpevents.Arguments{For: "example-user", IssueUID: grandchild.UID})
		require.NoError(t, service.Drain(ctx))

		move := func(from, to db.Project) {
			parent, err := store.IssueByUID(ctx, f.childIssue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{Single: true, IssueID: parent.ID, FromProjectID: from.ID, ToProjectID: to.ID, IfMatchRev: parent.Revision, Actor: "coordinator"})
			require.NoError(t, err)
			require.NoError(t, service.Drain(ctx))
		}
		move(f.public, f.hidden)
		assert.Equal(t, int64(1), got.pings.Load(), "moving the parent out")
		move(f.hidden, f.public)
		assert.Equal(t, int64(2), got.pings.Load(), "moving it back")
	})
}

// insertPayloadIdentityEvent records a pulled issue.metadata_updated whose
// issue identity is only in its payload, as some federated events carry it.
// The diff gives example-user a request.
func insertPayloadIdentityEvent(t *testing.T, store db.Storage, project db.Project, eventUID, issueUID string) {
	t.Helper()
	execer, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	payload := `{"issue_uid":"` + issueUID + `","diff":{"` + notification.MetadataKey("example-user") + `":{"to":{"from":"coordinator","message":"review"}}}}`
	_, err := execer.ExecContext(t.Context(), `INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
		type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
		VALUES($1, '01ARZ3NDEKTSV4RRFFQ69G5FAW', $2, $3, 'issue.metadata_updated', 'coordinator', $4, 1, 0, $5)`,
		eventUID, project.ID, project.Name, payload, strings.Repeat("a", 64))
	require.NoError(t, err)
}

// A federated event that names its issue only in the payload is matched and
// authorized against that issue: an issue filter hears it, and a scoped token
// does not hear one about an issue outside its subtree.
func TestMCPEventsPayloadIssueIdentityIsMatchedAndAuthorized(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		outside, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "outside the subtree", Author: "coordinator"})
		require.NoError(t, err)
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user", IssueUID: f.publicIssue.UID})
		subscribeEvents(t, service, f.scoped, mcpevents.Arguments{For: "scoped-recipient"})
		require.NoError(t, service.Drain(ctx))

		insertPayloadIdentityEvent(t, store, f.public, "01ARZ3NDEKTSV4RRFFQ69G5FD1", f.publicIssue.UID)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, []string{"example-user"}, got.recipients(), "the issue filter hears its issue")

		// scoped-recipient's request on an issue outside the token's subtree.
		execer := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		payload := `{"issue_uid":"` + outside.UID + `","diff":{"` + notification.MetadataKey("scoped-recipient") + `":{"to":{"from":"coordinator","message":"review"}}}}`
		_, err = execer.ExecContext(ctx, `INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
			type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
			VALUES('01ARZ3NDEKTSV4RRFFQ69G5FD2', '01ARZ3NDEKTSV4RRFFQ69G5FAW', $1, $2, 'issue.metadata_updated', 'coordinator', $3, 1, 0, $4)`,
			f.public.ID, f.public.Name, payload, strings.Repeat("a", 64))
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, []string{"example-user"}, got.recipients(), "the scoped token is not pinged for an issue it cannot see")
	})
}

// A purge or stream reset is pinged only to owners who can see the project it
// happened in. Others learn nothing about projects they cannot see.
func TestMCPEventsPurgePingsOnlyOwnersWhoCanSeeTheProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "member-user"})
		subscribeEvents(t, service, f.outsider, mcpevents.Arguments{For: "outsider-user"})
		require.NoError(t, service.Drain(ctx))

		_, err := store.PurgeIssue(ctx, f.hiddenIssue.ID, "admin", nil)
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, []string{"member-user"}, got.recipients())
	})
}

// Archiving and purging a project before the worker scans deletes the archive
// event, so only the purge remains to tell the worker. Owners that could see
// the project are still pinged.
func TestMCPEventsArchiveAndPurgeBeforeAScanStillPing(t *testing.T) {
	for name, explicit := range map[string]bool{"dynamic": false, "explicit": true} {
		t.Run(name, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newEventsFixture(t, store)
				ctx := t.Context()
				doomed, err := store.CreateProject(ctx, "doomed-project")
				require.NoError(t, err)
				var got pingRecorder
				service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
				args := mcpevents.Arguments{For: "example-user"}
				if explicit {
					args.ProjectUIDs = []string{f.public.UID, doomed.UID}
					sortStrings(args.ProjectUIDs)
				}
				subscribeEvents(t, service, f.member, args)
				require.NoError(t, service.Drain(ctx))

				_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: doomed.ID, Actor: "admin", Force: true})
				require.NoError(t, err)
				_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: doomed.ID, Actor: "admin"})
				require.NoError(t, err)
				require.NoError(t, service.Drain(ctx))
				assert.Equal(t, int64(1), got.pings.Load())
			})
		})
	}
}

// recordApplied records, as a federation pull's applying transaction does,
// that the given pulled events were applied.
func recordApplied(t *testing.T, store db.Storage, eventUIDs ...string) {
	t.Helper()
	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok)
	tx, err := beginner.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, db.RecordMCPEventsRematch(t.Context(), tx, eventUIDs))
	require.NoError(t, tx.Commit())
}

// A pulled event can be scanned before its pull creates the issue. It sends
// nothing then, and once the pull has applied it the request is delivered.
func TestMCPEventsAppliedPullOfANewIssuePings(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, service.Drain(ctx))

		const issueUID, eventUID = "01ARZ3NDEKTSV4RRFFQ69G5FC1", "01ARZ3NDEKTSV4RRFFQ69G5FC2"
		insertRemoteIssueCreated(t, store, f.public, eventUID, issueUID)
		require.NoError(t, service.Drain(ctx))
		assert.Zero(t, got.pings.Load(), "the issue does not exist yet")

		// Applying creates the issue without a request of its own, so only the
		// pulled event can ping.
		_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{UID: issueUID, ProjectID: f.public.ID, Title: "remote", Author: "coordinator"})
		require.NoError(t, err)
		recordApplied(t, store, eventUID)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load())
		_, _, _, pending, err := store.MCPEventsRematch(ctx)
		require.NoError(t, err)
		assert.False(t, pending, "the record is cleared once matched")
	})
}

// A pulled event whose issue never arrives, as when it is quarantined, holds
// nobody back: later requests are delivered at once to every subscriber.
func TestMCPEventsPulledEventForAMissingIssueHoldsNobody(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user"})
		subscribeEvents(t, service, f.outsider, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, service.Drain(ctx))

		insertRemoteIssueCreated(t, store, f.public, "01ARZ3NDEKTSV4RRFFQ69G5FC3", "01ARZ3NDEKTSV4RRFFQ69G5FC4")
		notifyIssue(t, store, f.public.ID, "after the pulled event")
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(2), got.pings.Load())
	})
}

// A pull commits remote events before it applies them, and an Events worker
// on another daemon sharing the store can match an update to an existing
// issue in between. Its ping then leads the receiver to read inbox state the
// pull has not applied yet. The applying transaction records a rematch that
// every daemon reads, so the receiver is pinged again after the update.
func TestMCPEventsAppliedPullPingsAgainOnAnyDaemon(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		keyPath := filepath.Join(t.TempDir(), "mcp-events.key")
		var early, late pingRecorder
		scanning, _ := newDaemonServiceWithKeys(t, store, f, mcpevents.NewFileKeyProvider(keyPath), early.respond)
		subscribeEvents(t, scanning, f.member, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, scanning.Drain(ctx))

		const eventUID = "01ARZ3NDEKTSV4RRFFQ69G5FE1"
		insertPayloadIdentityEvent(t, store, f.public, eventUID, f.publicIssue.UID)
		require.NoError(t, scanning.Drain(ctx))
		require.Equal(t, int64(1), early.pings.Load(), "pinged before the pull was applied")

		// The pull is applied by another daemon; a third delivers.
		recordApplied(t, store, eventUID)
		other, _ := newDaemonServiceWithKeys(t, store, f, mcpevents.NewFileKeyProvider(keyPath), late.respond)
		require.NoError(t, other.Drain(ctx))
		assert.Equal(t, int64(1), late.pings.Load(), "pinged again after the pull was applied")
	})
}
