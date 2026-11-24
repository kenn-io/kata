package federation

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
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

func TestRetainedCommentHistoryIgnoresEditOnlySnapshotEntries(t *testing.T) {
	const commentUID = "01HZNQ7VFPK1XGD8R5MABCD4ED"
	payload := []byte(`{"uid":"01HZNQ7VFPK1XGD8R5MABCD4EE","comments":[{"comment_uid":"` + commentUID + `","author":"earlier-actor","body":"edited placeholder"}]}`)
	require.False(t, createdComments("issue.snapshot", payload)[commentUID])

	payload = []byte(`{"uid":"01HZNQ7VFPK1XGD8R5MABCD4EE","comments":[{"comment_uid":"` + commentUID + `","author":"earlier-actor","body":"real comment","created_at":"2026-05-23T12:00:00Z"}]}`)
	require.True(t, createdComments("issue.snapshot", payload)[commentUID])
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

func TestRejoinReplyDependsOnRetainedParent(t *testing.T) {
	for _, tc := range []struct {
		name               string
		publishTask        bool
		publishParent      bool
		crossTask          bool
		pushOnly           bool
		independentTask    bool
		editParent         bool
		wantQuarantine     bool
		wantHistoryReadErr bool
	}{
		{name: "parent unpublished", publishTask: true, independentTask: true, wantQuarantine: true},
		{name: "parent accepted on another task", publishTask: true, publishParent: true, crossTask: true},
		{name: "unpublished task recovery snapshot"},
		{name: "push-only credential cannot check parent", publishTask: true, pushOnly: true, wantHistoryReadErr: true},
		{name: "edit before creation", publishTask: true, editParent: true, wantQuarantine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "reply-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			parentIssue := issue
			if tc.crossTask {
				parentIssue, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "parent task", Author: "old-actor"})
				require.NoError(t, err)
			}
			if tc.publishTask {
				require.NoError(t, spoke.sync(t, 0))
			}
			parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: parentIssue.ID, Author: "old-actor", Body: "parent"})
			require.NoError(t, err)
			if tc.publishParent {
				require.NoError(t, spoke.sync(t, 0))
			}
			capabilities := "push,pull"
			if tc.pushOnly {
				capabilities = "push"
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, capabilities)
			var independent db.Issue
			var independentEvent db.Event
			if tc.independentTask {
				independent, independentEvent, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "independent task", Author: "new-actor"})
				require.NoError(t, err)
			}
			var event db.Event
			if tc.editParent {
				var changed bool
				_, edit, changed, editErr := spoke.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: parentIssue.ID, CommentUID: parent.UID, Actor: "new-actor", Body: "edited"})
				require.NoError(t, editErr)
				require.True(t, changed)
				require.NotNil(t, edit)
				event = *edit
			} else {
				_, event, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
				require.NoError(t, err)
			}
			err = pushRejoin(t, spoke)
			if tc.wantHistoryReadErr {
				require.ErrorContains(t, err, "check retained discussion comment "+parent.UID)
				_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
				require.ErrorIs(t, err, db.ErrNotFound)
				binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
				require.NoError(t, err)
				require.Less(t, binding.PushCursorEventID, event.ID)
				return
			}
			if tc.wantQuarantine {
				require.ErrorContains(t, err, parent.UID)
				quarantine, err := spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
				require.NoError(t, err)
				require.Equal(t, []string{event.UID}, quarantine.EventUIDs)
				if tc.independentTask {
					binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
					require.NoError(t, err)
					require.GreaterOrEqual(t, binding.PushCursorEventID, independentEvent.ID)
					require.Less(t, binding.PushCursorEventID, event.ID)
					_, err = hub.DB.IssueByUID(ctx, independent.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
				}
				localEvents, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{event.UID})
				require.NoError(t, err)
				require.Len(t, localEvents, 1)
				require.Equal(t, "new-actor", localEvents[0].Actor)
				require.Equal(t, event.ContentHash, localEvents[0].ContentHash)
				if tc.editParent {
					remote, err := hub.DB.IssueByUID(ctx, parentIssue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
					require.NoError(t, err)
					require.Empty(t, comments)
				} else {
					remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
					require.NoError(t, err)
					require.Empty(t, comments)
				}
				return
			}
			require.NoError(t, err)
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
			require.NoError(t, err)
			if tc.crossTask {
				require.Len(t, comments, 1)
				require.Equal(t, "reply", comments[0].Body)
				require.Equal(t, parent.UID, comments[0].ReplyToUID)
			} else {
				require.Len(t, comments, 2)
				var bodies []string
				for _, comment := range comments {
					bodies = append(bodies, comment.Body)
					if comment.Body == "reply" {
						require.Equal(t, parent.UID, comment.ReplyToUID)
					}
				}
				require.ElementsMatch(t, []string{"parent", "reply"}, bodies)
			}
			if tc.crossTask {
				remoteParent, err := hub.DB.IssueByUID(ctx, parentIssue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				parentComments, err := hub.DB.CommentsByIssue(ctx, remoteParent.ID)
				require.NoError(t, err)
				require.Len(t, parentComments, 1)
				require.Equal(t, parent.UID, parentComments[0].UID)
			}
		})
	}
}

func TestRejoinPublishesRetainedLinkPeers(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
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
		{name: "cross project", run: func(t *testing.T) {
			for _, withCredential := range []bool{true, false} {
				name := "credential available"
				if !withCredential {
					name = "credential missing"
				}
				t.Run(name, func(t *testing.T) {
					ctx := t.Context()
					hub, projects := newMoveDeliveryHub(t, "source-project", "peer-project")
					spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
					issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
					require.NoError(t, err)
					peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "peer", Author: "old-actor"})
					require.NoError(t, err)
					parentIssue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "parent", Author: "old-actor"})
					require.NoError(t, err)
					parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: parentIssue.ID, Author: "old-actor", Body: "parent"})
					require.NoError(t, err)
					_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: peer.ID, Author: "old-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
					require.NoError(t, err)
					_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later source task", Author: "old-actor"})
					require.NoError(t, err)
					require.NoError(t, spoke.sync(t, 0))
					require.NoError(t, spoke.sync(t, 1))
					rejoinProjectWithActor(t, spoke, 0, hub.URL, projects[0], hub.DB, "push,pull")
					rejoinProjectWithActor(t, spoke, 1, hub.URL, projects[1], hub.DB, "push,pull")
					peerCredential := spoke.credentials[projects[1].UID]
					if !withCredential {
						require.NoError(t, spoke.credentials.DeleteFederationCredential(ctx, projects[1].UID))
					}
					_, linkEvent, err := spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "new-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
					require.NoError(t, err)
					err = pushRejoin(t, spoke)
					if !withCredential {
						require.ErrorContains(t, err, peer.UID)
						binding, bindingErr := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
						require.NoError(t, bindingErr)
						require.Less(t, binding.PushCursorEventID, linkEvent.ID)
						remoteIssue, issueErr := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
						require.NoError(t, issueErr)
						links, linksErr := hub.DB.LinksByIssue(ctx, remoteIssue.ID)
						require.NoError(t, linksErr)
						require.Empty(t, links)
						require.NoError(t, spoke.credentials.StoreFederationCredential(ctx, projects[1].UID, peerCredential))
						err = pushRejoin(t, spoke)
						require.NoError(t, err)
					}
					require.NoError(t, err)
					remotePeer, err := hub.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					remoteIssue, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					links, err := hub.DB.LinksByIssue(ctx, remoteIssue.ID)
					require.NoError(t, err)
					require.Len(t, links, 1)
					require.Equal(t, remotePeer.UID, links[0].ToIssueUID)
					// The peer binding later sees its locally recorded recovery snapshot.
					// It must advance past the copy the source binding already published.
					require.NoError(t, pushRejoinProject(t, spoke, 1))
				})
			}
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

func moveRetainedTaskHTTP(ctx context.Context, t *testing.T, spoke moveDeliverySpoke, issue db.Issue, targetUID, actor string, dryRun bool) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.ShortID),
		strings.NewReader(fmt.Sprintf(`{"actor":%q,"to_project_uid":%q,"dry_run":%t}`, actor, targetUID, dryRun)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
	resp, err := spoke.env.HTTP.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, string(body)
}

func TestRetainedTaskAfterRejoin(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		publishBeforeRejoin     bool
		secondRejoin            bool
		publishRecoverySnapshot bool
		restoreRecoverySnapshot bool
		initialMoveError        string
		finalMoveError          string
	}{
		{name: "previously-published", publishBeforeRejoin: true},
		{name: "unpublished-move", initialMoveError: "federation_sync_required"},
		{name: "published-snapshot", secondRejoin: true, publishRecoverySnapshot: true},
		{name: "restored-published-snapshot", secondRejoin: true, publishRecoverySnapshot: true, restoreRecoverySnapshot: true},
		{name: "unpublished-snapshot-after-published-issue", publishBeforeRejoin: true, secondRejoin: true, finalMoveError: "federated_move_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			actors := []string{"old-actor", "new-actor"}
			if tc.secondRejoin {
				actors[1] = "third-actor"
			}
			spoke := newMoveDeliverySpoke(t, hub, projects, actors...)
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained task", Author: "old-actor"})
			require.NoError(t, err)
			if !tc.secondRejoin {
				_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "other task", Author: "old-actor"})
				require.NoError(t, err)
			}
			if tc.publishBeforeRejoin && tc.secondRejoin {
				require.NoError(t, spoke.sync(t, 0))
			}
			if !tc.secondRejoin {
				_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "original comment"})
				require.NoError(t, err)
				if tc.publishBeforeRejoin {
					require.NoError(t, spoke.sync(t, 0))
				}
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull,claim")
			moveActor := "new-actor"
			if tc.secondRejoin {
				_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "recovered"})
				require.NoError(t, err)
				var snapshot db.Event
				if tc.publishRecoverySnapshot {
					require.NoError(t, spoke.sync(t, 0))
					if tc.restoreRecoverySnapshot {
						spoke = restoreMoveDeliverySpoke(t, spoke, hub, projects)
						issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
						require.NoError(t, err)
					}
				} else {
					snapshot, _, _, err = spoke.env.DB.RetainedIssueSnapshot(ctx, spoke.projects[0].ID, issue.UID)
					require.NoError(t, err)
				}
				_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
				require.NoError(t, err)
				grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull,claim", Actor: "third-actor"})
				require.NoError(t, err)
				_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true, PushEnabled: true, Actor: "third-actor", RetainHistory: true})
				require.NoError(t, err)
				spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "third-actor"}
				moveActor = "third-actor"
				if tc.publishRecoverySnapshot {
					require.NoError(t, spoke.sync(t, 0))
				} else {
					_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later task", Author: moveActor})
					require.NoError(t, err)
					require.NoError(t, spoke.sync(t, 0))
					binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
					require.NoError(t, err)
					require.Greater(t, binding.PushCursorEventID, snapshot.ID)
					remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Equal(t, "old-actor", remote.Author)
					comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
					require.NoError(t, err)
					require.Empty(t, comments)
				}
			}
			require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(ctx, projects[0].UID, spoke.credentials[projects[0].UID]))
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			if tc.initialMoveError != "" {
				status, body := moveRetainedTaskHTTP(ctx, t, spoke, issue, projects[1].UID, moveActor, true)
				require.Equal(t, http.StatusConflict, status, body)
				var preview api.ErrorEnvelope
				require.NoError(t, json.Unmarshal([]byte(body), &preview))
				require.Equal(t, tc.initialMoveError, preview.Error.Code)
				status, body = moveRetainedTaskHTTP(ctx, t, spoke, issue, projects[1].UID, moveActor, false)
				require.Equal(t, http.StatusConflict, status, body)
				var response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(body), &response))
				require.Equal(t, tc.initialMoveError, response.Error.Code)
				current, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, spoke.projects[0].ID, current.ProjectID)
				require.NoError(t, spoke.sync(t, 0))
			}
			if tc.finalMoveError != "" {
				status, body := moveRetainedTaskHTTP(ctx, t, spoke, issue, projects[1].UID, moveActor, true)
				require.Equal(t, http.StatusConflict, status, body)
				var preview api.ErrorEnvelope
				require.NoError(t, json.Unmarshal([]byte(body), &preview))
				require.Equal(t, tc.finalMoveError, preview.Error.Code)
			}
			status, body := moveRetainedTaskHTTP(ctx, t, spoke, issue, projects[1].UID, moveActor, false)
			if tc.finalMoveError != "" {
				require.Equal(t, http.StatusConflict, status, body)
				var response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(body), &response))
				require.Equal(t, tc.finalMoveError, response.Error.Code)
				current, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, spoke.projects[0].ID, current.ProjectID)
				return
			}
			require.Equal(t, http.StatusOK, status, body)
			if tc.initialMoveError != "" {
				_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "later", Author: moveActor})
				require.NoError(t, err)
			}
			require.NoError(t, spoke.sync(t, 0))
			require.NoError(t, spoke.sync(t, 1))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, remote.ProjectID)
		})
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
