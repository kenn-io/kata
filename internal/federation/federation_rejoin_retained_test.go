package federation

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

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
}

func TestRejoinRetainedMoveAfterRecovery(t *testing.T) {
	for _, originalPublished := range []bool{false, true} {
		name := "recovery"
		if originalPublished {
			name = "original history"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "move-source", "move-target")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
			source, target := spoke.projects[0], spoke.projects[1]
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "retained task", Author: "old-actor"})
			require.NoError(t, err)
			peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "retained peer", Author: "old-actor"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "old comment"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "old-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
			require.NoError(t, err)
			if originalPublished {
				require.NoError(t, spoke.sync(t, 0))
			}
			rejoinProjectWithActor(t, spoke, 0, hub.URL, projects[0], hub.DB, "push,pull")
			rejoinProjectWithActor(t, spoke, 1, hub.URL, projects[1], hub.DB, "push,pull")
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "first edit"})
			require.NoError(t, err)
			require.NoError(t, pushRejoin(t, spoke))
			if !originalPublished {
				snapshots, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, IssueUID: issue.UID, Types: []string{"issue.snapshot"}, Limit: 10})
				require.NoError(t, err)
				require.Len(t, snapshots, 1)
				binding, err := spoke.env.DB.FederationBindingByProject(ctx, source.ID)
				require.NoError(t, err)
				require.GreaterOrEqual(t, binding.PushCursorEventID, snapshots[0].ID)
			}
			before, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, Limit: 100})
			require.NoError(t, err)
			current, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			for _, dryRun := range []bool{true, false} {
				_, err = spoke.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: current.Revision, Actor: "new-actor", DryRun: dryRun})
				require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
			}
			unchanged, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, source.ID, unchanged.ProjectID)
			require.Equal(t, current.Revision, unchanged.Revision)
			after, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, Limit: 100})
			require.NoError(t, err)
			require.Equal(t, before, after)
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "later edit"})
			require.NoError(t, err)
			later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "later task", Author: "new-actor"})
			require.NoError(t, err)
			require.NoError(t, pushRejoin(t, spoke))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[0].ID, remote.ProjectID)
			comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, comments, 3)
			require.Equal(t, []string{"old comment", "first edit", "later edit"}, []string{comments[0].Body, comments[1].Body, comments[2].Body})
			links, err := hub.DB.LinksByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, peer.UID, links[0].ToIssueUID)
			_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
		})
	}
}

func TestRejoinRetainedBatchRecoveryIsLinear(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "batch-recovery")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	const count = 24
	issues := make([]db.Issue, 0, count)
	for range count {
		issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained task", Author: "old-actor"})
		require.NoError(t, err)
		issues = append(issues, issue)
	}
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	for _, issue := range issues {
		_, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "edit"})
		require.NoError(t, err)
	}
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	creds := spoke.credentials[projects[0].UID]
	client, err := NewClient(ctx, creds.HubURL, creds.Token, clientpkg.Opts{})
	require.NoError(t, err)
	transport := client.client.Transport
	submitted, rejected := 0, 0
	client.client.Transport = pushOnlyTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/federation/events:ingest") {
			body, err := req.GetBody()
			require.NoError(t, err)
			var request api.FederationIngestEventsRequestBody
			require.NoError(t, json.UnmarshalRead(body, &request))
			require.NoError(t, body.Close())
			submitted += len(request.Events)
		}
		resp, err := transport.RoundTrip(req)
		if err == nil && resp.StatusCode == http.StatusBadRequest {
			rejected++
		}
		return resp, err
	})
	require.NoError(t, pushDirectSpoke(ctx, spoke.env.DB, &binding, projects[0].ID, client, nil, nil, spoke.credentials, clientpkg.Opts{}))
	require.Greater(t, rejected, 0)
	require.LessOrEqual(t, submitted, 5*count)
	for _, issue := range issues {
		remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
	}
}
