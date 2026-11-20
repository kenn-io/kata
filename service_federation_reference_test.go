package kata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/testenv"
)

type projectPushController struct{}

func (projectPushController) AuthorizeFederation(context.Context, FederationAccessRequest) (FederationAccessDecision, error) {
	return FederationAccessDecision{TransactionFence: func(context.Context, Transaction) error { return nil }}, nil
}

type projectReferenceController struct {
	projectPushController
	allow atomic.Bool
}

func (c *projectReferenceController) AuthorizeFederationReference(_ context.Context, _ Transaction, route, peer FederationAccessRequest) error {
	if route.Project.UID == peer.Project.UID || route.Enrollment.SpokeInstanceUID != peer.Enrollment.SpokeInstanceUID ||
		route.Enrollment.Actor != "worker" || peer.Enrollment.Actor != "peer-member" || !c.allow.Load() {
		return ErrAccessDenied
	}
	return nil
}

func TestDirectPushWithProjectScopedActors(t *testing.T) {
	for _, mode := range []string{"standalone", "existing-controller", "host-principal", "host"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			controller := &projectReferenceController{}
			cfg := Config{DSN: filepath.Join(t.TempDir(), "hub.db"), Auth: AuthConfig{TrustCallerAuthentication: true}}
			switch mode {
			case "existing-controller":
				cfg.FederationAccess = projectPushController{}
			case "host", "host-principal":
				cfg.FederationAccess = controller
			}
			if mode == "host-principal" {
				cfg.Auth = AuthConfig{}
				cfg.Access = allowWorkerFenceAccess{}
			}
			hub, err := New(ctx, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, hub.Close()) })
			handler := hub.Handler()
			if mode == "host-principal" {
				mounted := handler
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mounted.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{Subject: "user", Actor: "worker"})))
				})
				controller.allow.Store(true)
			}
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			spoke := testenv.New(t)
			credentials := serviceCredentialStoreAdapter{newServiceFederationCredentialStore()}
			var projects []db.Project
			var bindings []db.FederationBinding
			for i, actor := range []string{"worker", "peer-member"} {
				name := []string{"route-project", "peer-project"}[i]
				project, err := hub.store.CreateProject(ctx, name)
				require.NoError(t, err)
				projects = append(projects, project)
				grant, err := hub.CreateFederationEnrollment(ctx, FederationEnrollmentSpec{ProjectUID: project.UID, SpokeInstanceUID: spoke.DB.InstanceUID(), Capabilities: "pull,push", Actor: actor})
				require.NoError(t, err)
				local, err := spoke.DB.CreateProjectWithUID(ctx, name, project.UID)
				require.NoError(t, err)
				binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: server.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: actor})
				require.NoError(t, err)
				bindings = append(bindings, binding)
				require.NoError(t, credentials.store.StoreFederationCredential(ctx, project.UID, FederationCredential{HubURL: server.URL, HubProjectID: project.ID, Token: grant.Token, Actor: actor}))
			}
			syncProject := func(i int) error {
				b, err := spoke.DB.FederationBindingByProject(ctx, bindings[i].ProjectID)
				if err != nil {
					return err
				}
				cred, _, err := credentials.FederationCredential(ctx, b.HubProjectUID)
				if err != nil {
					return err
				}
				return federation.SyncFederationOnce(ctx, spoke.DB, b, cred, credentials)
			}
			peer, _, err := hub.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "Peer issue", Author: "peer-member"})
			require.NoError(t, err)
			other, _, err := hub.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Existing child", Author: "worker"})
			require.NoError(t, err)
			require.NoError(t, syncProject(0))
			require.NoError(t, syncProject(1))
			local, err := spoke.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			child, err := spoke.DB.IssueByUID(ctx, other.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			created, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: bindings[0].ProjectID, Title: "new child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: local.ID}, {Type: "blocks", ToNumber: local.ID, Incoming: true}}})
			require.NoError(t, err)
			_, err = spoke.DB.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: child.ID, Actor: "worker", SetParent: &local.ID})
			require.NoError(t, err)
			before, err := spoke.DB.FederationBindingByProject(ctx, child.ProjectID)
			require.NoError(t, err)
			pending := func() []db.Event {
				events, err := spoke.DB.PendingFederationPushEvents(ctx, child.ProjectID, spoke.DB.InstanceUID(), before.PushCursorEventID, 100)
				require.NoError(t, err)
				return events
			}
			original := pending()
			require.Len(t, original, 2)
			require.ErrorContains(t, syncProject(0), "403")
			_, err = hub.store.IssueByUID(ctx, created.UID, db.IncludeDeletedNo)
			require.ErrorIs(t, err, db.ErrNotFound)
			unchanged, err := spoke.DB.FederationBindingByProject(ctx, child.ProjectID)
			require.NoError(t, err)
			require.Equal(t, before.PushCursorEventID, unchanged.PushCursorEventID)
			require.Equal(t, original, pending())
			if mode != "host" {
				return
			}
			controller.allow.Store(true)
			require.NoError(t, syncProject(0))
			after, err := spoke.DB.FederationBindingByProject(ctx, child.ProjectID)
			require.NoError(t, err)
			require.Equal(t, original[len(original)-1].ID, after.PushCursorEventID)
			require.Equal(t, before.CreatedAt, after.CreatedAt)
			parent, err := hub.store.ParentOf(ctx, other.ID)
			require.NoError(t, err)
			require.Equal(t, peer.ID, parent.ToIssueID)
			got, err := hub.store.IssueByUID(ctx, created.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			links, err := hub.store.LinksByIssue(ctx, got.ID)
			require.NoError(t, err)
			require.Len(t, links, 2)
			for _, link := range links {
				switch link.Type {
				case "parent":
					require.Equal(t, got.ID, link.FromIssueID)
					require.Equal(t, peer.ID, link.ToIssueID)
				case "blocks":
					require.Equal(t, peer.ID, link.FromIssueID)
					require.Equal(t, got.ID, link.ToIssueID)
				default:
					t.Fatalf("unexpected link type %q", link.Type)
				}
			}
			stored, err := hub.store.EventsByUIDs(ctx, projects[0].ID, []string{original[0].UID, original[1].UID})
			require.NoError(t, err)
			require.Len(t, stored, len(original))
			for i, event := range stored {
				require.Equal(t, original[i].ContentHash, event.ContentHash)
				require.Equal(t, original[i].Payload, event.Payload)
				require.Equal(t, original[i].Actor, event.Actor)
			}

			t.Run("direct-link-with-different-actors", func(t *testing.T) {
				directChild, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: child.ProjectID, Title: "Direct child", Author: "worker"})
				require.NoError(t, err)
				require.NoError(t, syncProject(0))
				_, linked, err := spoke.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: directChild.ID, ToIssueID: local.ID, Type: "parent", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: directChild.ID, FromShortID: directChild.ShortID, FromUID: directChild.UID, ToShortID: local.ShortID, ToUID: local.UID, Actor: "worker"})
				require.NoError(t, err)
				later, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: child.ProjectID, Title: "Later direct link work", Author: "worker"})
				require.NoError(t, err)
				require.NoError(t, syncProject(0))
				hubChild, err := hub.store.IssueByUID(ctx, directChild.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				parent, err := hub.store.ParentOf(ctx, hubChild.ID)
				require.NoError(t, err)
				require.Equal(t, peer.ID, parent.ToIssueID)
				_, err = hub.store.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				events, err := hub.store.EventsByUIDs(ctx, projects[0].ID, []string{linked.UID})
				require.NoError(t, err)
				require.Len(t, events, 1)
			})

			t.Run("delete-peer-before-link-push", func(t *testing.T) {
				_, linkedEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: child.ProjectID, Title: "Queued link", Author: "worker",
					Links: []db.InitialLink{{Type: "related", ToNumber: local.ID}},
				})
				require.NoError(t, err)
				_, _, _, err = spoke.DB.SoftDeleteIssue(ctx, local.ID, "peer-member")
				require.NoError(t, err)
				require.NoError(t, syncProject(1))
				deleted, err := hub.store.IssueByUID(ctx, peer.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.NotNil(t, deleted.DeletedAt)

				_, laterEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: child.ProjectID, Title: "Later work", Author: "worker"})
				require.NoError(t, err)
				require.NoError(t, syncProject(0))
				got, err := hub.store.EventsByUIDs(ctx, projects[0].ID, []string{linkedEvent.UID, laterEvent.UID})
				require.NoError(t, err)
				require.Len(t, got, 2)
				require.Equal(t, linkedEvent.ContentHash, got[0].ContentHash)
				require.Equal(t, laterEvent.ContentHash, got[1].ContentHash)
				binding, err := spoke.DB.FederationBindingByProject(ctx, child.ProjectID)
				require.NoError(t, err)
				require.Equal(t, laterEvent.ID, binding.PushCursorEventID)
			})
		})
	}
}
