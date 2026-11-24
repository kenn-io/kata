package federation

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func rejoinWithActor(t *testing.T, spoke moveDeliverySpoke, hubURL string, hubProject db.Project, hubStore db.Storage, capabilities string) {
	t.Helper()
	ctx := t.Context()
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	grant, err := hubStore.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &hubProject.ID, Capabilities: capabilities, Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hubURL, HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[hubProject.UID] = config.FederationCredential{HubURL: hubURL, HubProjectID: hubProject.ID, Token: grant.Token, Actor: "new-actor"}
}

func TestRejoinRejectsWrongActor(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "wrong-actor-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	binding.Actor = "old-actor"
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	_, wrong, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "wrong actor task", Author: "old-actor"})
	require.NoError(t, err)
	require.Equal(t, "old-actor", wrong.Actor)
	require.Greater(t, wrong.ID, binding.RetainedThroughEventID)
	binding.Actor = "new-actor"
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	err = spoke.sync(t, 0)
	require.ErrorContains(t, err, "does not match bound actor")
	_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
}

func pushRejoin(t *testing.T, spoke moveDeliverySpoke) error {
	t.Helper()
	ctx := t.Context()
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	if err != nil {
		return err
	}
	creds := spoke.credentials[spoke.projects[0].UID]
	client, err := NewClient(ctx, creds.HubURL, creds.Token, clientpkg.Opts{})
	if err != nil {
		return err
	}
	return pushDirectSpoke(ctx, spoke.env.DB, &binding, creds.HubProjectID, client, nil, nil, spoke.credentials, clientpkg.Opts{})
}

func TestRejoinRetainedIssueAndLaterTaskPublish(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	old, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, commentEvent, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local edit"})
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
	require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, spoke.projects[0].ID, 0, 0))
	require.NoError(t, spoke.sync(t, 0))
	events, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{commentEvent.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestRejoinedSpokeBatchesNewTasks(t *testing.T) {
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
}

func TestRejoinRetainedIssueKeepsCrossProjectLinkLocal(t *testing.T) {
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
	require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, spoke.projects[0].ID, 0, 0))
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
}

func TestRetainedTaskAfterRejoin(t *testing.T) {
	for _, mode := range []string{"previously-published", "unpublished-move"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "new-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "other task", Author: "old-actor"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "original comment"})
			require.NoError(t, err)
			if mode == "previously-published" {
				require.NoError(t, spoke.sync(t, 0))
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull,claim")
			require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(ctx, projects[0].UID, spoke.credentials[projects[0].UID]))
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			move := func() (int, string) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.ShortID), strings.NewReader(fmt.Sprintf(`{"actor":"new-actor","to_project_uid":%q}`, projects[1].UID)))
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
			if mode == "unpublished-move" {
				status, body := move()
				require.Equal(t, http.StatusConflict, status, body)
				var response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(body), &response))
				require.Equal(t, "federation_sync_required", response.Error.Code)
				current, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, spoke.projects[0].ID, current.ProjectID)
				require.NoError(t, spoke.sync(t, 0))
			}
			status, body := move()
			require.Equal(t, http.StatusOK, status, body)
			var later db.Issue
			if mode == "unpublished-move" {
				later, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "later", Author: "new-actor"})
				require.NoError(t, err)
			}
			require.NoError(t, spoke.sync(t, 0))
			require.NoError(t, spoke.sync(t, 1))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, remote.ProjectID)
			if mode == "unpublished-move" {
				remote, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, projects[1].ID, remote.ProjectID)
			}
		})
	}
}

func TestMoveRetainedTaskAfterSecondRejoin(t *testing.T) {
	for _, tc := range []struct {
		name              string
		publishedSnapshot bool
		publishedIssue    bool
	}{
		{name: "published-snapshot", publishedSnapshot: true},
		{name: "unpublished-snapshot"},
		{name: "unpublished-snapshot-after-published-issue", publishedIssue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "third-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained task", Author: "old-actor"})
			require.NoError(t, err)
			if tc.publishedIssue {
				require.NoError(t, spoke.sync(t, 0))
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull,claim")
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "recovered"})
			require.NoError(t, err)
			var snapshot db.Event
			if tc.publishedSnapshot {
				require.NoError(t, spoke.sync(t, 0))
			} else {
				snapshot, _, err = spoke.env.DB.RetainedIssueSnapshot(ctx, spoke.projects[0].ID, issue.UID)
				require.NoError(t, err)
			}
			_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull,claim", Actor: "third-actor"})
			require.NoError(t, err)
			_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true, PushEnabled: true, Actor: "third-actor", RetainHistory: true})
			require.NoError(t, err)
			spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "third-actor"}
			if !tc.publishedSnapshot {
				_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later task", Author: "third-actor"})
				require.NoError(t, err)
			}
			require.NoError(t, spoke.sync(t, 0))
			if !tc.publishedSnapshot {
				binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
				require.NoError(t, err)
				require.Greater(t, binding.PushCursorEventID, snapshot.ID)
				remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				if tc.publishedIssue {
					require.NoError(t, err)
					require.Equal(t, "old-actor", remote.Author)
					comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
					require.NoError(t, err)
					require.Empty(t, comments)
				} else {
					require.ErrorIs(t, err, db.ErrNotFound)
				}
			}
			require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(ctx, projects[0].UID, spoke.credentials[projects[0].UID]))
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.ShortID), strings.NewReader(fmt.Sprintf(`{"actor":"third-actor","to_project_uid":%q}`, projects[1].UID)))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
			resp, err := spoke.env.HTTP.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			if !tc.publishedSnapshot {
				require.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
				var response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(body, &response))
				if tc.publishedIssue {
					require.Equal(t, "federated_move_unsupported", response.Error.Code)
				} else {
					require.Equal(t, "federation_sync_required", response.Error.Code)
				}
				current, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, spoke.projects[0].ID, current.ProjectID)
				return
			}
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			require.NoError(t, spoke.sync(t, 0))
			require.NoError(t, spoke.sync(t, 1))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, remote.ProjectID)
		})
	}
}
