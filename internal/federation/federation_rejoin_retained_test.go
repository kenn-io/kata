package federation

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/testenv"
)

func TestRejoinRefusesRetainedTaskMove(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "move-source", "move-target")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "new-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull,claim")
	for _, dryRun := range []bool{true, false} {
		body := fmt.Sprintf(`{"actor":"new-actor","to_project_uid":%q,"dry_run":%t}`, projects[1].UID, dryRun)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.ShortID), strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
		resp, err := spoke.env.HTTP.Do(req)
		require.NoError(t, err)
		var response api.ErrorEnvelope
		require.NoError(t, json.UnmarshalRead(resp.Body, &response))
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		require.Equal(t, "federated_move_unsupported", response.Error.Code)
	}
}

func rejoinWithActor(t *testing.T, spoke moveDeliverySpoke, hubURL string, hubProject db.Project, hubStore db.Storage, capabilities string) {
	rejoinProjectWithActor(t, spoke, 0, hubURL, hubProject, hubStore, capabilities)
}

func rejoinProjectWithActor(t *testing.T, spoke moveDeliverySpoke, projectIndex int, hubURL string, hubProject db.Project, hubStore db.Storage, capabilities string) {
	t.Helper()
	ctx := t.Context()
	localProject := spoke.projects[projectIndex]
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, localProject.ID)
	require.NoError(t, err)
	grant, err := hubStore.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &hubProject.ID, Capabilities: capabilities, Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: localProject.ID, Role: db.FederationRoleSpoke, HubURL: hubURL, HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[hubProject.UID] = config.FederationCredential{HubURL: hubURL, HubProjectID: hubProject.ID, Token: grant.Token, Actor: "new-actor"}
}

func restoreMoveDeliverySpoke(t *testing.T, spoke moveDeliverySpoke, hub *testenv.Env, projects []db.Project) moveDeliverySpoke {
	t.Helper()
	ctx := t.Context()
	records, err := dbtest.CollectImportRecords(ctx, spoke.env.DB, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	spoke.env = testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.FederationPushPending = DirectSpokePushPending(cfg.DB, cfg.Publish().Events)
	})
	require.NoError(t, spoke.env.DB.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
	for i, project := range projects {
		spoke.projects[i], err = spoke.env.DB.ProjectByUID(ctx, project.UID)
		require.NoError(t, err)
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[i].ID)
		require.NoError(t, err)
		grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "push,pull,claim", Actor: binding.Actor})
		require.NoError(t, err)
		spoke.credentials[project.UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token, Actor: binding.Actor}
	}
	return spoke
}

func pushRejoin(t *testing.T, spoke moveDeliverySpoke) error {
	return pushRejoinProject(t, spoke, 0)
}

func pushRejoinProject(t *testing.T, spoke moveDeliverySpoke, projectIndex int) error {
	t.Helper()
	ctx := t.Context()
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[projectIndex].ID)
	if err != nil {
		return err
	}
	creds := spoke.credentials[spoke.projects[projectIndex].UID]
	client, err := NewClient(ctx, creds.HubURL, creds.Token, clientpkg.Opts{})
	if err != nil {
		return err
	}
	return pushDirectSpoke(ctx, spoke.env.DB, &binding, creds.HubProjectID, client, nil, nil, spoke.credentials, clientpkg.Opts{})
}

func TestRejoinPublishesRetainedLinkPeers(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "rejoin original actor", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "repeat-rejoin")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "first edit"})
			require.NoError(t, err)
			require.NoError(t, pushRejoin(t, spoke))
			_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull", Actor: "old-actor"})
			require.NoError(t, err)
			_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true, PushEnabled: true, Actor: "old-actor", RetainHistory: true})
			require.NoError(t, err)
			spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "old-actor"}
			later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later", Author: "old-actor"})
			require.NoError(t, err)
			require.NoError(t, pushRejoin(t, spoke))
			_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
		}},
		{name: "edited blocked task", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "blocked-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			blocker, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "blocker", Author: "old-actor"})
			require.NoError(t, err)
			blocked, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "blocked", Author: "old-actor"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: blocker.ID, ToIssueID: blocked.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: blocker.ID, Actor: "old-actor", FromUID: blocker.UID, ToUID: blocked.UID, FromShortID: blocker.ShortID, ToShortID: blocked.ShortID})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: blocked.ID, Author: "new-actor", Body: "update"})
			require.NoError(t, err)
			require.NoError(t, pushRejoin(t, spoke))
			remoteBlocker, err := hub.DB.IssueByUID(ctx, blocker.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			links, err := hub.DB.LinksByIssue(ctx, remoteBlocker.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, blocked.UID, links[0].ToIssueUID)
		}},
		{name: "link", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "link-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "peer", Author: "old-actor"})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "new-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
			require.NoError(t, err)
			require.NoError(t, pushRejoin(t, spoke))
			remotePeer, err := hub.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			links, err := hub.DB.LinksByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, remotePeer.UID, links[0].ToIssueUID)
		}},
		{name: "set parent", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "bulk-link-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained parent", Author: "old-actor"})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			child, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "new child", Author: "new-actor"})
			require.NoError(t, err)
			_, err = spoke.env.DB.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: child.ID, Actor: "new-actor", SetParent: &peer.ID})
			require.NoError(t, err)
			require.NoError(t, pushRejoin(t, spoke))
			remoteChild, err := hub.DB.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			remotePeer, err := hub.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			links, err := hub.DB.LinksByIssue(ctx, remoteChild.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, remotePeer.UID, links[0].ToIssueUID)
		}},
		{name: "cycle", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "cyclic-link-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			first, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "first", Author: "old-actor"})
			require.NoError(t, err)
			second, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "second", Author: "old-actor"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: first.ID, ToIssueID: second.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: first.ID, Actor: "old-actor", FromUID: first.UID, ToUID: second.UID, FromShortID: first.ShortID, ToShortID: second.ShortID})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: second.ID, ToIssueID: first.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: second.ID, Actor: "old-actor", FromUID: second.UID, ToUID: first.UID, FromShortID: second.ShortID, ToShortID: first.ShortID})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			newIssue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "new task", Author: "new-actor"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: newIssue.ID, ToIssueID: first.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: newIssue.ID, Actor: "new-actor", FromUID: newIssue.UID, ToUID: first.UID, FromShortID: newIssue.ShortID, ToShortID: first.ShortID})
			require.NoError(t, err)
			require.NoError(t, pushRejoin(t, spoke))
			for _, issue := range []db.Issue{first, second, newIssue} {
				_, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err, issue.Title)
			}
			remoteNewIssue, err := hub.DB.IssueByUID(ctx, newIssue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			links, err := hub.DB.LinksByIssue(ctx, remoteNewIssue.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, first.UID, links[0].ToIssueUID)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestRejoinRetainedIssueAndLaterTaskPublish(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "later task", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "rejoin-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			old, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local edit"})
			require.NoError(t, err)
			later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later task", Author: "new-actor"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			remote, err := hub.DB.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "local edit", comments[0].Body)
			_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
		}},
		{name: "independent restore", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "restore-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "new-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained task", Author: "new-actor"})
			require.NoError(t, err)
			spoke = restoreMoveDeliverySpoke(t, spoke, hub, projects)
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "new comment"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "new comment", comments[0].Body)
		}},
		{name: "batch new tasks", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "batch-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			_, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			require.NoError(t, pushRejoin(t, spoke))
			for _, title := range []string{"first", "second"} {
				_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: title, Author: "new-actor"})
				require.NoError(t, err)
			}
			binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			creds := spoke.credentials[projects[0].UID]
			client, err := NewClient(ctx, creds.HubURL, creds.Token, clientpkg.Opts{})
			require.NoError(t, err)
			transport := client.client.Transport
			requests := 0
			client.client.Transport = pushOnlyTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					requests++
				}
				return transport.RoundTrip(req)
			})
			require.NoError(t, pushDirectSpoke(ctx, spoke.env.DB, &binding, projects[0].ID, client, nil, nil, spoke.credentials, clientpkg.Opts{}))
			require.Equal(t, 1, requests)
		}},
		{name: "local cross-project link", run: func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "rejoin-project", "peer-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
			peerProject := spoke.projects[1]
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained task", Author: "old-actor"})
			require.NoError(t, err)
			peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peerProject.ID, Title: "peer task", Author: "old-actor"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 1))
			_, linkEvent, err := spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "old-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "later edit"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			remoteLinks, err := hub.DB.LinksByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Empty(t, remoteLinks)
			hubBinding, err := hub.DB.FederationBindingByProject(ctx, projects[0].ID)
			require.NoError(t, err)
			horizon := hubBinding.ReplayHorizonEventID
			require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, spoke.projects[0].ID, horizon, max(horizon-1, 0)))
			require.NoError(t, spoke.sync(t, 0))
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			links, err := spoke.env.DB.LinksByIssue(ctx, issue.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, peer.UID, links[0].ToIssueUID)
			events, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{linkEvent.UID})
			require.NoError(t, err)
			require.Len(t, events, 1)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestRejoinPublishesRecoverySnapshot(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "recovery-publication")
	var push func(context.Context, db.FederationBinding, config.FederationCredential) error
	options := []testenv.Option{func(cfg *daemon.ServerConfig) { push = cfg.FederationPushPending }}
	spoke := newMoveDeliverySpokeWithOptions(t, hub, projects, options, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "new comment"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	sub := spoke.env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: spoke.projects[0].ID})
	defer sub.Unsub()
	require.NoError(t, push(ctx, binding, spoke.credentials[projects[0].UID]))
	select {
	case msg := <-sub.Ch:
		require.NotNil(t, msg.Event)
		require.Equal(t, "issue.snapshot", msg.Event.Type)
	default:
		t.Fatal("recovery snapshot was not published")
	}
	_, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}
