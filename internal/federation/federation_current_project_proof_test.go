package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// The sender can still see the old owner when another joined spoke has already
// moved the issue on the hub. Supplemental proofs cannot rely on local ownership.
func TestFederationOrdinarySourceProofIncludesJoinedCurrentProject(t *testing.T) {
	for _, kind := range []string{"same-origin", "other-binding-origin", "other-credential-origin", "other-actor", "pull-only", "relay"} {
		t.Run(kind, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "proof-source", "proof-target")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Stale local owner", Author: "worker"})
			require.NoError(t, err)
			_, event, err := spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Delayed source edit"})
			require.NoError(t, err)
			other := testenv.New(t)
			target, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[1].ID)
			require.NoError(t, err)
			switch kind {
			case "other-binding-origin":
				target.HubURL = other.URL
			case "other-credential-origin":
				credential := spoke.credentials[projects[1].UID]
				credential.HubURL = other.URL
				spoke.credentials[projects[1].UID] = credential
			case "other-actor":
				target.Actor = "other-worker"
			case "pull-only":
				target.PushEnabled = false
			}
			_, err = spoke.env.DB.UpsertFederationBinding(t.Context(), target)
			require.NoError(t, err)
			if kind == "relay" {
				bindingUID, err := uid.New()
				require.NoError(t, err)
				public, _, err := ed25519.GenerateKey(rand.Reader)
				require.NoError(t, err)
				require.NoError(t, spoke.env.DB.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: projects[1].UID, AuthorityUID: hub.DB.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				_, err = spoke.env.DB.SetRelayBindingConfig(t.Context(), target.ProjectID, db.RelayBindingConfig{
					ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
					UpstreamInstanceUID: hub.DB.InstanceUID(), AuthorityUID: hub.DB.InstanceUID(),
					HubPath:    []string{hub.DB.InstanceUID(), spoke.env.DB.InstanceUID()},
					LocalActor: "worker", ResetEpoch: 1,
				})
				require.NoError(t, err)
			}
			binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
			require.NoError(t, err)
			tokens, err := federationMoveSourceTokens(t.Context(), spoke.env.DB, spoke.credentials, binding, hub.URL, []db.Event{event}, clientpkg.Opts{})
			if kind == "other-credential-origin" {
				require.Error(t, err, "credentials are never sent to a different configured origin")
				require.Empty(t, tokens)
				return
			}
			require.NoError(t, err)
			if kind == "same-origin" {
				require.Equal(t, spoke.credentials[projects[1].UID].Token, tokens[projects[1].UID])
			} else {
				require.NotContains(t, tokens, projects[1].UID)
			}
		})
	}
}

type boundedPushPageStore struct {
	db.Storage
	limit int
}

func (s *boundedPushPageStore) PendingFederationPushEvents(ctx context.Context, projectID int64, origin string, after int64, limit int) ([]db.Event, error) {
	if s.limit > 0 {
		limit = min(limit, s.limit)
	} else {
		limit = 1
	}
	return s.Storage.PendingFederationPushEvents(ctx, projectID, origin, after, limit)
}

func TestSyncFederationMissingEnrollmentDoesNotBlockOtherPushes(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
	sender := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved work", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, sender.sync(t, 0))
	local, err := sender.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, commentEvent, err := sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Retained stale comment"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	good, _, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Independent first batch", Author: "worker"})
	require.NoError(t, err)
	paged := &boundedPushPageStore{Storage: sender.env.DB}
	push := func(wantPending bool) {
		t.Helper()
		binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
		require.NoError(t, err)
		client, err := NewClient(t.Context(), hub.URL, sender.credentials[projects[0].UID].Token, clientpkg.Opts{})
		require.NoError(t, err)
		err = pushDirectSpoke(t.Context(), paged, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{})
		if wantPending {
			require.ErrorContains(t, err, "pending federation events remain")
		} else {
			require.NoError(t, err)
		}
	}
	push(true)
	_, err = hub.DB.IssueByUID(t.Context(), good.UID, db.IncludeDeletedNo)
	require.NoError(t, err, "independent work arrives after an all-rejected page")
	status, err := sender.env.DB.FederationSyncStatusByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	require.NotNil(t, status.LastError)
	require.Contains(t, *status.LastError, projects[1].Name)
	require.Contains(t, *status.LastError, issue.UID)
	binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	require.Less(t, binding.PushCursorEventID, commentEvent.ID, "retain the original rejected event for retry")
	// Cron forwarding uses the exported drain contract, while ordinary sync
	// continues through retained per-event rejections via the private phase.
	client, err := NewClient(t.Context(), hub.URL, sender.credentials[projects[0].UID].Token, clientpkg.Opts{})
	require.NoError(t, err)
	require.ErrorContains(t, PushDirectSpoke(t.Context(), sender.env.DB, binding, projects[0].ID, client), "pending federation events remain", "a partial push is not a fully drained cron prerequisite")
	later, _, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Independent later turn", Author: "worker"})
	require.NoError(t, err)
	push(true)
	_, err = hub.DB.IssueByUID(t.Context(), later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Empty(t, comments)

	// Joining the destination alone must recover the retained event next turn.
	target, err := sender.env.DB.CreateProjectWithUID(t.Context(), projects[1].Name, projects[1].UID)
	require.NoError(t, err)
	_, err = sender.env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: sender.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull", Actor: "worker"})
	require.NoError(t, err)
	sender.credentials[projects[1].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "worker"}
	push(false)
	comments, err = hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "Retained stale comment", comments[0].Body)

}

func TestPushDefersDependentEventsAcrossPages(t *testing.T) {
	for _, mode := range []string{"pages", "body-splits"} {
		t.Run(mode, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			sender := newMoveDeliverySpoke(t, hub, projects[:1])
			definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
			require.NoError(t, err)
			job, _, err := hub.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: projects[0].ID, Name: "Review", Actor: "worker", Definition: definition})
			require.NoError(t, err)
			moved, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved", Author: "worker"})
			require.NoError(t, err)
			require.NoError(t, sender.sync(t, 0))
			local, err := sender.env.DB.IssueByUID(t.Context(), moved.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			body := ""
			if mode == "body-splits" {
				body = strings.Repeat("x", 400<<10)
			}
			created, _, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Dependent creation", Body: body, Author: "worker", Links: []db.InitialLink{{ToNumber: local.ID, Type: "related"}}})
			require.NoError(t, err)
			runUID, err := uid.New()
			require.NoError(t, err)
			_, err = sender.env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: sender.projects[0].ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, IssueUID: &created.UID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
			require.NoError(t, err)
			_, _, err = sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: created.ID, Author: "worker", Body: "Dependent comment" + body})
			require.NoError(t, err)
			child, _, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Dependent child", Author: "worker", Links: []db.InitialLink{{ToNumber: created.ID, Type: "related"}}})
			require.NoError(t, err)
			_, _, err = sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: child.ID, Author: "worker", Body: "Child comment"})
			require.NoError(t, err)
			independent, independentEvent, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Independent", Author: "worker"})
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: moved.Revision, Actor: "worker"})
			require.NoError(t, err)
			binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
			require.NoError(t, err)
			client, err := NewClient(t.Context(), hub.URL, sender.credentials[projects[0].UID].Token, clientpkg.Opts{})
			require.NoError(t, err)
			var pushStore db.Storage = sender.env.DB
			if mode == "pages" {
				pushStore = &boundedPushPageStore{Storage: sender.env.DB}
			}
			err = pushDirectSpoke(t.Context(), pushStore, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{})
			require.ErrorContains(t, err, "pending federation events remain")
			accepted, err := sender.env.DB.FederationPushAccepted(t.Context(), binding.ProjectID, independentEvent.ID)
			require.NoError(t, err)
			require.True(t, accepted)
			_, err = hub.DB.IssueByUID(t.Context(), independent.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.IssueByUID(t.Context(), created.UID, db.IncludeDeletedNo)
			require.ErrorIs(t, err, db.ErrNotFound)
			_, err = hub.DB.CronRun(t.Context(), projects[0].ID, runUID)
			require.ErrorIs(t, err, db.ErrNotFound)
			err = pushDirectSpoke(t.Context(), pushStore, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{})
			require.ErrorContains(t, err, "pending federation events remain")
			target, err := sender.env.DB.CreateProjectWithUID(t.Context(), projects[1].Name, projects[1].UID)
			require.NoError(t, err)
			_, err = sender.env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID, Enabled: true, PushEnabled: true, Actor: "worker"})
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: sender.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull", Actor: "worker"})
			require.NoError(t, err)
			sender.credentials[projects[1].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "worker"}
			require.NoError(t, pushDirectSpoke(t.Context(), pushStore, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{}))
			require.GreaterOrEqual(t, binding.PushCursorEventID, independentEvent.ID)
			run, err := hub.DB.CronRun(t.Context(), projects[0].ID, runUID)
			require.NoError(t, err)
			require.Equal(t, &created.UID, run.IssueUID)
			got, err := hub.DB.IssueByUID(t.Context(), child.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(t.Context(), got.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "Child comment", comments[0].Body)
		})
	}
}

func TestSyncFederationPartialPushAndPull(t *testing.T) {
	for _, destinationBound := range []bool{true, false} {
		name := "destination bound"
		if !destinationBound {
			name = "destination unbound"
		}
		t.Run(name, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			bound := projects[:1]
			if destinationBound {
				bound = projects
			}
			sender := newMoveDeliverySpoke(t, hub, bound)
			issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved work", Author: "worker"})
			require.NoError(t, err)
			for i := range bound {
				require.NoError(t, sender.sync(t, i))
			}
			local, err := sender.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, _, err = sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Delayed edit"})
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			delete(sender.credentials, projects[1].UID)
			binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
			require.NoError(t, err)
			runner := &Runner{DB: sender.env.DB, Credentials: sender.credentials}
			_, err = runner.runSpoke(t.Context(), activeSpokeBinding{binding: binding, project: sender.projects[0]}, sender.credentials, nil, nil)
			require.ErrorContains(t, err, "pending federation events remain")
			require.ErrorContains(t, err, "matching push access")
			if destinationBound {
				after, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
				require.NoError(t, err)
				require.Greater(t, after.PullCursorEventID, binding.PullCursorEventID)
				return
			}
			require.ErrorContains(t, err, "need project access")
			require.ErrorIs(t, err, db.ErrNotFound)
			status, err := sender.env.DB.FederationSyncStatusByProject(t.Context(), sender.projects[0].ID)
			require.NoError(t, err)
			require.NotNil(t, status.LastError)
			require.Contains(t, *status.LastError, "not found")
			require.Contains(t, *status.LastError, "pending federation events remain")
			require.Contains(t, *status.LastError, "need project access")
		})
	}
}

func TestFederationMissingEnrollmentThenPoisonBlocksPush(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "poison-source", "poison-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved work", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, stale, err := spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Retain this event"})
	require.NoError(t, err)
	dependent, creation, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Retained creation", Author: "worker", Links: []db.InitialLink{{ToNumber: local.ID, Type: "related"}}})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, poison, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Malformed secondary reference", Author: "worker"})
	require.NoError(t, err)
	_, comment, err := spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: dependent.ID, Author: "worker", Body: "Retained dependency"})
	require.NoError(t, err)
	// Corrupt only this synthetic retained event, as a real 400 poison delivery.
	_, err = spoke.env.DB.ExecContext(t.Context(), `UPDATE events SET related_issue_uid = ? WHERE id = ?`, "01HZNQ7VFPK1XGD8R5MABCD4EB", poison.ID)
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Wait behind poison", Author: "worker"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
	require.NoError(t, err)
	client, err := NewClient(t.Context(), hub.URL, spoke.credentials[projects[0].UID].Token, clientpkg.Opts{})
	require.NoError(t, err)
	paged := &boundedPushPageStore{Storage: spoke.env.DB, limit: 2}
	err = pushDirectSpoke(t.Context(), paged, &binding, projects[0].ID, client, nil, spoke.credentials, clientpkg.Opts{})
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, 400, status.StatusCode)
	q, err := spoke.env.DB.ActiveFederationQuarantine(t.Context(), binding.ProjectID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	require.Equal(t, []string{stale.UID, creation.UID, poison.UID, comment.UID}, q.EventUIDs)
	require.Equal(t, stale.ID, q.FirstEventID)
	require.Equal(t, comment.ID, q.LastEventID)
}

// Matching local echoes must not publish a mutation twice merely because an
// unrelated rejection holds the retry cursor behind work the hub accepted.
func TestFederationRejectedPrefixDoesNotRedeliverAcceptedLocalEcho(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "echo-source", "echo-target")
	sender := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved work", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, sender.sync(t, i))
	}
	local, err := sender.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, rejected, err := sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Pending enrollment"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	delete(sender.credentials, projects[1].UID)
	_, accepted, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Independent accepted work", Author: "worker"})
	require.NoError(t, err)
	binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	var delivered []string
	err = SyncFederationOnceWithPulledEvents(t.Context(), sender.env.DB, binding, sender.credentials[projects[0].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
		for _, event := range events {
			delivered = append(delivered, event.UID)
		}
	}, sender.credentials)
	require.ErrorContains(t, err, "pending federation events remain")
	binding, err = sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	require.Less(t, binding.PushCursorEventID, rejected.ID)
	_, err = hub.DB.EventsByUIDs(t.Context(), projects[0].ID, []string{accepted.UID})
	require.NoError(t, err, "the independent event was accepted")
	require.NotContains(t, delivered, accepted.UID, "a local mutation was already delivered when authored")
}

func TestFederationPullOnlySuppressesAcceptedEchoAfterReopen(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "echo-source", "echo-target")
	sender := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved work", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, sender.sync(t, i))
	}
	local, err := sender.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, rejected, err := sender.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Pending enrollment"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	delete(sender.credentials, projects[1].UID)
	_, accepted, err := sender.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Independent", Author: "worker"})
	require.NoError(t, err)
	binding, err := sender.env.DB.FederationBindingByProject(ctx, sender.projects[0].ID)
	require.NoError(t, err)
	client, err := NewClient(ctx, hub.URL, sender.credentials[projects[0].UID].Token, clientpkg.Opts{})
	require.NoError(t, err)
	require.ErrorContains(t, pushDirectSpoke(ctx, sender.env.DB, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{}), "pending federation events remain")
	require.Less(t, binding.PushCursorEventID, rejected.ID)

	reopened, err := sqlitestore.Open(ctx, filepath.Join(sender.env.Home, "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	binding, err = reopened.FederationBindingByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	var delivered []string
	err = syncDirectSpoke(ctx, reopened, binding, false, projects[0].ID, client, func(_ int64, events []db.Event) {
		for _, event := range events {
			delivered = append(delivered, event.UID)
		}
	}, nil, sender.credentials, clientpkg.Opts{})
	require.NoError(t, err)
	require.NotContains(t, delivered, accepted.UID)
}
