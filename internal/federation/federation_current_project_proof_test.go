package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// The sender can still see the old owner when another joined spoke has already
// moved the issue on the hub. Supplemental proofs cannot rely on local ownership.
func TestFederationOrdinarySourceProofIncludesJoinedCurrentProject(t *testing.T) {
	for _, kind := range []string{"same-origin", "unavailable-signing-key", "other-binding-origin", "other-credential-origin", "other-actor", "pull-only", "relay"} {
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
			case "unavailable-signing-key":
				t.Setenv("KATA_TEST_MISSING_SIGNING_KEY", "")
				credential := spoke.credentials[projects[1].UID]
				credential.Signing = &signingpolicy.Source{KeyID: "test-key", KeyEnv: "KATA_TEST_MISSING_SIGNING_KEY", HubURL: hub.URL}
				spoke.credentials[projects[1].UID] = credential
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
			if kind == "same-origin" || kind == "unavailable-signing-key" {
				require.Equal(t, spoke.credentials[projects[1].UID].Token, tokens[projects[1].UID])
			} else {
				require.NotContains(t, tokens, projects[1].UID)
			}
		})
	}
}

func joinMoveTarget(t *testing.T, hub *testenv.Env, project db.Project, sender moveDeliverySpoke) {
	t.Helper()
	target, err := sender.env.DB.CreateProjectWithUID(t.Context(), project.Name, project.UID)
	require.NoError(t, err)
	_, err = sender.env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: sender.env.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker"})
	require.NoError(t, err)
	sender.credentials[project.UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token, Actor: "worker"}
}

type boundedPushPageStore struct {
	db.Storage
}

func (s *boundedPushPageStore) PendingFederationPushEvents(ctx context.Context, projectID int64, origin string, after int64, _ int) ([]db.Event, error) {
	return s.Storage.PendingFederationPushEvents(ctx, projectID, origin, after, 1)
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
	joinMoveTarget(t, hub, projects[1], sender)
	push(false)
	comments, err = hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "Retained stale comment", comments[0].Body)

}

func newPushDeferralFixture(t *testing.T, joinTarget bool) (*testenv.Env, []db.Project, moveDeliverySpoke, db.Issue, db.Issue) {
	t.Helper()
	hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
	joined := projects[:1]
	if joinTarget {
		joined = projects
	}
	spoke := newMoveDeliverySpoke(t, hub, joined)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	return hub, projects, spoke, issue, local
}

func TestPushDefersDependentEventsAcrossPages(t *testing.T) {
	for _, mode := range []string{"pages", "body-splits"} {
		t.Run(mode, func(t *testing.T) {
			hub, projects, sender, moved, local := newPushDeferralFixture(t, false)
			body := ""
			if mode == "body-splits" {
				body = strings.Repeat("x", 400<<10)
			}
			created, _, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Dependent creation", Body: body, Author: "worker", Links: []db.InitialLink{{ToNumber: local.ID, Type: "related"}}})
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
			pending, pendingErr := sender.env.DB.PendingFederationPushEvents(t.Context(), binding.ProjectID, sender.env.DB.InstanceUID(), binding.PushCursorEventID, 100)
			require.NoError(t, pendingErr)
			require.ErrorContains(t, err, fmt.Sprintf("%d events need project access", len(pending)))
			accepted, err := sender.env.DB.FederationPushAccepted(t.Context(), binding.ProjectID, independentEvent.ID)
			require.NoError(t, err)
			require.True(t, accepted)
			_, err = hub.DB.IssueByUID(t.Context(), independent.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.IssueByUID(t.Context(), created.UID, db.IncludeDeletedNo)
			require.ErrorIs(t, err, db.ErrNotFound)
			err = pushDirectSpoke(t.Context(), pushStore, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{})
			require.ErrorContains(t, err, "pending federation events remain")
			joinMoveTarget(t, hub, projects[1], sender)
			require.NoError(t, pushDirectSpoke(t.Context(), pushStore, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{}))
			require.GreaterOrEqual(t, binding.PushCursorEventID, independentEvent.ID)
			got, err := hub.DB.IssueByUID(t.Context(), child.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(t.Context(), got.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "Child comment", comments[0].Body)
		})
	}
}

func TestPushDefersRejectedMoveDependentsAcrossPages(t *testing.T) {
	for _, mode := range []string{"missing", "leave", "enrollment", "disabled", "uploads-disabled"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			hub, projects, spoke, issue, local := newPushDeferralFixture(t, true)
			moved := spoke.move(t, local, 0, 1)
			const commentBody = "Work after moving"
			_, comment, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: moved.ID, Author: "worker", Body: commentBody})
			require.NoError(t, err)
			independent, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "Independent", Author: "worker"})
			require.NoError(t, err)

			binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[1].ID)
			require.NoError(t, err)
			client, err := NewClient(ctx, hub.URL, spoke.credentials[projects[1].UID].Token, clientpkg.Opts{})
			require.NoError(t, err)
			pushStore := &boundedPushPageStore{Storage: spoke.env.DB}
			pending, err := pushStore.PendingFederationPushEvents(ctx, binding.ProjectID, pushStore.InstanceUID(), binding.PushCursorEventID, federationPollLimit)
			require.NoError(t, err)
			require.NotEmpty(t, pending)
			move := pending[0]
			require.Equal(t, "issue.moved", move.Type)

			// The hub receives the move without proof for its source project.
			sourceProof := spoke.credentials[projects[0].UID]
			sourceBinding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			switch mode {
			case "missing":
				delete(spoke.credentials, projects[0].UID)
			case "leave":
				credential := sourceProof
				credential.LeavePending = true
				spoke.credentials[projects[0].UID] = credential
			case "enrollment":
				credential := sourceProof
				credential.RelayEnrollmentPending = true
				spoke.credentials[projects[0].UID] = credential
			case "disabled", "uploads-disabled":
				changed := sourceBinding
				changed.Enabled = mode != "disabled"
				changed.PushEnabled = mode != "uploads-disabled"
				_, err = spoke.env.DB.UpsertFederationBinding(ctx, changed)
				require.NoError(t, err)
			}
			push := func() error {
				return pushDirectSpoke(ctx, pushStore, &binding, projects[1].ID, client, nil, spoke.credentials, clientpkg.Opts{})
			}
			pushErr := push()
			require.ErrorContains(t, pushErr, "pending federation events remain")
			if mode != "missing" {
				require.ErrorContains(t, pushErr, projects[0].UID)
			}
			require.Less(t, binding.PushCursorEventID, move.ID)
			got, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[0].ID, got.ProjectID)
			comments, err := hub.DB.CommentsByIssue(ctx, got.ID)
			require.NoError(t, err)
			require.Empty(t, comments)
			_, err = hub.DB.IssueByUID(ctx, independent.UID, db.IncludeDeletedNo)
			require.NoError(t, err, "unrelated work can pass the retained move and comment")
			_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, binding.ProjectID, db.FederationQuarantineDirectionPush)
			require.ErrorIs(t, err, db.ErrNotFound)

			spoke.credentials[projects[0].UID] = sourceProof
			if mode == "disabled" || mode == "uploads-disabled" {
				_, err = spoke.env.DB.UpsertFederationBinding(ctx, sourceBinding)
				require.NoError(t, err)
			}
			require.NoError(t, push())
			require.GreaterOrEqual(t, binding.PushCursorEventID, comment.ID)
			got, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, got.ProjectID)
			comments, err = hub.DB.CommentsByIssue(ctx, got.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, commentBody, comments[0].Body)
		})
	}
}
func TestSyncFederationPartialPushAndPull(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
	bound := projects[:1]
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
	binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	upstream, err := url.Parse(hub.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/events") {
			http.Error(w, "pull temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	binding.HubURL = server.URL
	_, err = sender.env.DB.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	credential := sender.credentials[projects[0].UID]
	credential.HubURL = server.URL
	sender.credentials[projects[0].UID] = credential
	runner := &Runner{DB: sender.env.DB, Credentials: sender.credentials}
	_, err = runner.runSpoke(t.Context(), activeSpokeBinding{binding: binding, project: sender.projects[0]}, sender.credentials, nil, nil)
	require.ErrorContains(t, err, "pending federation events remain")
	require.ErrorContains(t, err, "pull temporarily unavailable")
	status, err := sender.env.DB.FederationSyncStatusByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	require.NotNil(t, status.LastError)
	require.Contains(t, *status.LastError, "pull temporarily unavailable")
	require.Contains(t, *status.LastError, "pending federation events remain")
	require.Contains(t, *status.LastError, "need project access")
}

func TestFederationSuppressesAcceptedLocalEcho(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		name := "same turn"
		if reopen {
			name = "after reopen"
		}
		t.Run(name, func(t *testing.T) {
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
			previousPull := binding.PullCursorEventID
			var delivered []string
			callback := func(_ int64, events []db.Event) {
				for _, event := range events {
					delivered = append(delivered, event.UID)
				}
			}
			if reopen {
				client, err := NewClient(ctx, hub.URL, sender.credentials[projects[0].UID].Token, clientpkg.Opts{})
				require.NoError(t, err)
				require.ErrorContains(t, pushDirectSpoke(ctx, sender.env.DB, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{}), "pending federation events remain")
				reopened, err := sqlitestore.Open(ctx, filepath.Join(sender.env.Home, "kata.db"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = reopened.Close() })
				binding, err = reopened.FederationBindingByProject(ctx, binding.ProjectID)
				require.NoError(t, err)
				require.NoError(t, syncDirectSpoke(ctx, reopened, binding, false, projects[0].ID, client, callback, nil, sender.credentials, clientpkg.Opts{}))
				binding, err = reopened.FederationBindingByProject(ctx, binding.ProjectID)
				require.NoError(t, err)
				require.Greater(t, binding.PullCursorEventID, previousPull)
			} else {
				err = SyncFederationOnceWithPulledEvents(ctx, sender.env.DB, binding, sender.credentials[projects[0].UID], clientpkg.Opts{}, callback, sender.credentials)
				require.ErrorContains(t, err, "pending federation events remain")
				binding, err = sender.env.DB.FederationBindingByProject(ctx, sender.projects[0].ID)
				require.NoError(t, err)
				require.Greater(t, binding.PullCursorEventID, previousPull)
			}
			require.Less(t, binding.PushCursorEventID, rejected.ID)
			_, err = hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{accepted.UID})
			require.NoError(t, err)
			require.NotContains(t, delivered, accepted.UID)
		})
	}
}
