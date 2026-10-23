package daemon_test

import (
	"context"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
)

func TestMCPEventsPrincipalFromContext(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		p    daemon.Principal
		want mcpevents.Principal
	}{
		{"db token", daemon.Principal{Kind: daemon.PrincipalDBToken, TokenID: 42, Actor: "example-user"},
			mcpevents.Principal{Authority: "db_token", Subject: "42", Actor: "example-user"}},
		{"static token", daemon.Principal{Kind: daemon.PrincipalStaticToken, Subject: "credential-fingerprint"},
			mcpevents.Principal{Authority: "static_token", Subject: "credential-fingerprint"}},
		{"host keeps the exact subject bytes", daemon.Principal{Kind: daemon.PrincipalHost, Subject: " Tenant/One ", Actor: "example-user"},
			mcpevents.Principal{Authority: "host", Subject: " Tenant/One ", Actor: "example-user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := daemon.MCPEventsPrincipalFromContext(daemon.WithPrincipal(ctx, tc.p))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	for _, p := range []daemon.Principal{
		{Kind: daemon.PrincipalBootstrap},
		{Kind: daemon.PrincipalStaticToken},
		{Kind: daemon.PrincipalTrustedProxy, Actor: "example-user"},
		{Kind: daemon.PrincipalTrustedProxyAbsent},
		{Kind: daemon.PrincipalWebLocal},
		{Kind: daemon.PrincipalDBToken, Actor: "example-user"}, // no token id
		{Kind: daemon.PrincipalHost, Subject: " ", Actor: "example-user"},
		{Kind: daemon.PrincipalHost, Subject: "tenant"}, // no actor
	} {
		_, err := daemon.MCPEventsPrincipalFromContext(daemon.WithPrincipal(ctx, p))
		assert.ErrorIs(t, err, mcpevents.ErrForbidden, string(p.Kind))
	}
	_, err := daemon.MCPEventsPrincipalFromContext(ctx)
	assert.ErrorIs(t, err, mcpevents.ErrForbidden, "a request with no principal and no owner authority")

	a, _ := daemon.MCPEventsPrincipalFromContext(daemon.WithPrincipal(ctx, daemon.Principal{Kind: daemon.PrincipalHost, Subject: "tenant-1", Actor: "shared"}))
	b, _ := daemon.MCPEventsPrincipalFromContext(daemon.WithPrincipal(ctx, daemon.Principal{Kind: daemon.PrincipalHost, Subject: "tenant-2", Actor: "shared"}))
	assert.NotEqual(t, a.Key(), b.Key())
}

type hostAccess struct {
	err        error
	revalidate func(context.Context) error
	requests   []daemon.HostAccessRequest
}

func (h *hostAccess) Authorize(_ context.Context, req daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	h.requests = append(h.requests, req)
	return daemon.HostAccessDecision{Revalidate: h.revalidate}, h.err
}

func TestMCPEventsReacquire(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		future := time.Now().UTC().Add(time.Hour)
		live, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "live-token", Actor: "example-user", AdminActor: db.BootstrapActor})
		require.NoError(t, err)
		revoked, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "revoked-token", Actor: "example-user", AdminActor: db.BootstrapActor})
		require.NoError(t, err)
		_, _, err = store.RevokeAPIToken(ctx, revoked.ID, db.BootstrapActor)
		require.NoError(t, err)
		expiring, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "expiring-token", Actor: "example-user", AdminActor: db.BootstrapActor, ExpiresAt: &future}) //nolint:gosec // Deterministic fixture credential, never an operational secret.
		require.NoError(t, err)

		dbPrincipal := func(id int64) mcpevents.Principal {
			return mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: itoa(id), Actor: "example-user"}
		}
		identity := func(cfg daemon.ServerConfig) mcpevents.Authority {
			cfg.DB = store
			return daemon.NewMCPEventsAuthority(cfg)
		}
		tokenCfg := daemon.ServerConfig{Auth: config.AuthConfig{Token: "configured", RequireTokenIdentity: true}}

		t.Run("db token", func(t *testing.T) {
			auth := identity(tokenCfg)
			_, err := auth.Reacquire(ctx, dbPrincipal(live.ID))
			require.NoError(t, err)
			_, err = auth.Reacquire(ctx, dbPrincipal(expiring.ID))
			require.NoError(t, err)
			for _, p := range []mcpevents.Principal{
				dbPrincipal(revoked.ID), dbPrincipal(live.ID + 1000),
				{Authority: mcpevents.AuthorityDBToken, Subject: "not-a-number"},
				{Authority: mcpevents.AuthorityDBToken, Subject: "0"},
				{Authority: "bootstrap", Subject: "x"},
			} {
				_, err := auth.Reacquire(ctx, p)
				assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked, p.Key())
			}
		})
		t.Run("a db token needs identity mode", func(t *testing.T) {
			// Outside identity mode database tokens no longer authenticate, the
			// counterpart of a static token downgraded to bootstrap.
			for _, cfg := range []daemon.ServerConfig{{}, {Auth: config.AuthConfig{Token: "configured"}}} {
				_, err := identity(cfg).Reacquire(ctx, dbPrincipal(live.ID))
				assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
			}
		})
		t.Run("static token follows the authentication mode", func(t *testing.T) {
			static := mcpevents.Principal{Authority: mcpevents.AuthorityStaticToken, Subject: "20158224750041d653cd8c737d64c2fdce258d4280c9bf2a49fa1db0a7f9f3ac"}
			_, err := identity(daemon.ServerConfig{Auth: config.AuthConfig{Token: "configured"}}).Reacquire(ctx, static)
			require.NoError(t, err)
			// Identity mode resolves the configured token to bootstrap, which
			// cannot subscribe.
			_, err = identity(tokenCfg).Reacquire(ctx, static)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
			_, err = identity(daemon.ServerConfig{}).Reacquire(ctx, static)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked, "no configured token")
		})
		t.Run("local follows the local mode", func(t *testing.T) {
			local := mcpevents.Principal{Authority: mcpevents.AuthorityLocal, Subject: "local"}
			_, err := identity(daemon.ServerConfig{}).Reacquire(ctx, local)
			require.NoError(t, err)
			_, err = identity(daemon.ServerConfig{Auth: config.AuthConfig{Token: "configured"}}).Reacquire(ctx, local)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
			_, err = identity(daemon.ServerConfig{HostAccess: &hostAccess{}}).Reacquire(ctx, local)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
		})
		t.Run("host asks the controller with the persisted subject and actor", func(t *testing.T) {
			host := mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1", Actor: "example-user"}
			_, err := identity(daemon.ServerConfig{}).Reacquire(ctx, host)
			require.ErrorIs(t, err, mcpevents.ErrAuthorityUnavailable, "a missing controller fails closed")
			assert.NotErrorIs(t, err, mcpevents.ErrPrincipalRevoked)

			controller := &hostAccess{}
			_, err = identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			require.NoError(t, err)
			require.Equal(t, []daemon.HostAccessRequest{{
				Subject: host.Subject, Actor: host.Actor,
				Operation: daemon.HostOperation{
					ID: "pollEvents", Method: http.MethodGet, Path: "/api/v1/events", AllProjects: true,
					Policy: daemon.HostOperationPolicy{Kind: "task_read", Capability: "read"},
				},
			}}, controller.requests)

			controller.err = daemon.ErrHostAccessDenied
			_, err = identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)

			controller.err = errors.New("controller offline")
			_, err = identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			require.ErrorIs(t, err, mcpevents.ErrAuthorityUnavailable)
			assert.NotErrorIs(t, err, mcpevents.ErrPrincipalRevoked)

			_, err = identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1"})
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked, "a host principal needs an actor")
		})
		t.Run("a later host revocation is noticed on use", func(t *testing.T) {
			revoked := false
			controller := &hostAccess{revalidate: func(context.Context) error {
				if revoked {
					return daemon.ErrHostAccessDenied
				}
				return nil
			}}
			host := mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1", Actor: "example-user"}
			authorized, err := identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			require.NoError(t, err)
			_, err = authorized.VisibleProjects(ctx, nil)
			require.NoError(t, err)
			revoked = true
			_, err = authorized.VisibleProjects(ctx, nil)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
		})
		t.Run("a temporary host outage remains retryable on use", func(t *testing.T) {
			var leaseErr error
			controller := &hostAccess{revalidate: func(context.Context) error { return leaseErr }}
			host := mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1", Actor: "example-user"}
			authorized, err := identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			require.NoError(t, err)
			_, err = authorized.VisibleProjects(ctx, nil)
			require.NoError(t, err)

			leaseErr = errors.New("controller offline")
			_, err = authorized.VisibleProjects(ctx, nil)
			require.ErrorIs(t, err, mcpevents.ErrAuthorityUnavailable)
			assert.NotErrorIs(t, err, mcpevents.ErrPrincipalRevoked)

			leaseErr = nil
			_, err = authorized.VisibleProjects(ctx, nil)
			require.NoError(t, err, "the same authorization remains usable after recovery")
		})
		t.Run("missing context never allows", func(t *testing.T) {
			_, err := daemon.NewMCPEventsAuthority(daemon.ServerConfig{}).Reacquire(ctx, mcpevents.Principal{Authority: mcpevents.AuthorityLocal, Subject: "local"})
			assert.ErrorIs(t, err, mcpevents.ErrAuthorityUnavailable, "no store")
		})
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// eventsFixture has a public and a team-only project, each with an issue, and
// principals with different views of them.
type eventsFixture struct {
	store          db.Storage
	public, hidden db.Project
	publicIssue    db.Issue
	hiddenIssue    db.Issue
	member         mcpevents.Principal
	outsider       mcpevents.Principal
	scoped         mcpevents.Principal
	childIssue     db.Issue
	authority      mcpevents.Authority
}

func newEventsFixture(t *testing.T, store db.Storage) *eventsFixture {
	t.Helper()
	ctx := t.Context()
	f := &eventsFixture{store: store}
	var err error
	f.public, err = store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	f.hidden, err = store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	team, _, err := store.CreateTeam(ctx, "project-team", db.BootstrapActor)
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "member-actor", true, db.BootstrapActor)
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: f.hidden.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, db.BootstrapActor)
	require.NoError(t, err)

	key := notification.MetadataKey("example-user")
	notify := map[string]jsontext.Value{key: jsontext.Value(`{"from":"example-user","message":"work"}`)}
	f.publicIssue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "public", Author: "coordinator", Metadata: notify})
	require.NoError(t, err)
	f.hiddenIssue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.hidden.ID, Title: "hidden", Author: "coordinator", Metadata: notify})
	require.NoError(t, err)
	f.childIssue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "child", Author: "coordinator", Links: []db.InitialLink{{Type: "parent", ToNumber: f.publicIssue.ID}}})
	require.NoError(t, err)

	token := func(plain, actor string, scope *db.APITokenScope) mcpevents.Principal {
		in := db.CreateAPITokenParams{PlaintextToken: plain, Actor: actor, AdminActor: db.BootstrapActor, Scope: scope}
		if scope != nil {
			expiry := time.Now().UTC().Add(time.Hour)
			in.ExpiresAt = &expiry
		}
		created, _, err := store.CreateAPIToken(ctx, in)
		require.NoError(t, err)
		return mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: itoa(created.ID), Actor: actor}
	}
	f.member = token("member-token", "member-actor", nil)
	f.outsider = token("outsider-token", "outsider-actor", nil)
	f.scoped = token("scoped-token", "scoped-actor", &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: f.publicIssue.UID})
	f.authority = daemon.NewMCPEventsAuthority(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "configured", RequireTokenIdentity: true}})
	return f
}

func (f *eventsFixture) as(t *testing.T, p mcpevents.Principal) mcpevents.Authorized {
	t.Helper()
	authorized, err := f.authority.Reacquire(t.Context(), p)
	require.NoError(t, err)
	return authorized
}

func projectUIDs(projects []mcpevents.Project) []string {
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		out = append(out, p.UID)
	}
	return out
}

func TestMCPEventsAuthorizedVisibleProjects(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		both := []string{f.public.UID, f.hidden.UID}
		if both[0] > both[1] {
			both[0], both[1] = both[1], both[0]
		}

		visible, err := f.as(t, f.member).VisibleProjects(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, both, projectUIDs(visible), "a team member sees both, sorted by UID")

		visible, err = f.as(t, f.outsider).VisibleProjects(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{f.public.UID}, projectUIDs(visible), "project policy hides the team project")

		visible, err = f.as(t, f.outsider).VisibleProjects(ctx, []string{f.hidden.UID})
		require.NoError(t, err)
		assert.Empty(t, visible, "an explicit selection cannot widen visibility")

		visible, err = f.as(t, f.member).VisibleProjects(ctx, []string{f.hidden.UID})
		require.NoError(t, err)
		assert.Equal(t, []string{f.hidden.UID}, projectUIDs(visible))

		visible, err = f.as(t, f.scoped).VisibleProjects(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{f.public.UID}, projectUIDs(visible), "a scoped credential sees only its project")
		visible, err = f.as(t, f.scoped).VisibleProjects(ctx, []string{f.hidden.UID})
		require.NoError(t, err)
		assert.Empty(t, visible)

		// The static-token and owner-local principals see every project.
		for _, cfg := range []struct {
			cfg daemon.ServerConfig
			p   mcpevents.Principal
		}{
			{daemon.ServerConfig{Auth: config.AuthConfig{Token: "configured"}}, mcpevents.Principal{Authority: mcpevents.AuthorityStaticToken, Subject: "20158224750041d653cd8c737d64c2fdce258d4280c9bf2a49fa1db0a7f9f3ac"}},
			{daemon.ServerConfig{}, mcpevents.Principal{Authority: mcpevents.AuthorityLocal, Subject: "local"}},
		} {
			cfg.cfg.DB = store
			authorized, err := daemon.NewMCPEventsAuthority(cfg.cfg).Reacquire(ctx, cfg.p)
			require.NoError(t, err)
			visible, err = authorized.VisibleProjects(ctx, nil)
			require.NoError(t, err)
			assert.Equal(t, both, projectUIDs(visible), cfg.p.Key())
		}

		// Archiving a project removes it from the catalog.
		_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: f.public.ID, Actor: db.BootstrapActor, Force: true})
		require.NoError(t, err)
		visible, err = f.as(t, f.outsider).VisibleProjects(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, visible)
	})
}

func TestMCPEventsAuthorizedHeadAndRead(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		outsider := f.as(t, f.outsider)
		member := f.as(t, f.member)

		maxID, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		head, err := outsider.Head(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, maxID, head)

		memberProjects, err := member.VisibleProjects(ctx, nil)
		require.NoError(t, err)
		outsiderProjects, err := outsider.VisibleProjects(ctx, nil)
		require.NoError(t, err)

		page, err := member.Read(ctx, memberProjects, 0, 1000)
		require.NoError(t, err)
		assert.False(t, page.Reset)
		assert.Equal(t, maxID, page.Next)
		seenHidden := false
		for _, o := range page.Occurrences {
			seenHidden = seenHidden || o.ProjectUID == f.hidden.UID
			assert.Positive(t, o.EventID)
			assert.NotEmpty(t, o.EventUID)
		}
		assert.True(t, seenHidden, "the member sees the team project's events")

		page, err = outsider.Read(ctx, outsiderProjects, 0, 1000)
		require.NoError(t, err)
		require.NotEmpty(t, page.Occurrences)
		for _, o := range page.Occurrences {
			assert.Equal(t, f.public.UID, o.ProjectUID, "policy hides the team project's events")
		}
		assert.Equal(t, maxID, page.Next, "a short read has seen everything through the head")

		// Naming the hidden project does not reveal it.
		page, err = outsider.Read(ctx, memberProjects, 0, 1000)
		require.NoError(t, err)
		for _, o := range page.Occurrences {
			assert.Equal(t, f.public.UID, o.ProjectUID)
		}

		page, err = member.Read(ctx, memberProjects, 0, 2)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(page.Occurrences), 2)
		assert.Less(t, page.Next, maxID, "a small limit returns a partial window")

		// A selection of one project filters in the reader.
		page, err = member.Read(ctx, memberProjects[:1], 0, 1000)
		require.NoError(t, err)
		for _, o := range page.Occurrences {
			assert.Equal(t, memberProjects[0].UID, o.ProjectUID)
		}
	})
}

func TestMCPEventsAuthorizedHeadIncludesPurgeResets(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		member := f.as(t, f.member)
		projects, err := member.VisibleProjects(ctx, nil)
		require.NoError(t, err)

		before, err := member.Head(ctx, projects)
		require.NoError(t, err)
		purge, err := store.PurgeIssue(ctx, f.childIssue.ID, "admin", nil)
		require.NoError(t, err)
		require.NotNil(t, purge.PurgeResetAfterEventID)

		head, err := member.Head(ctx, projects)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, head, *purge.PurgeResetAfterEventID, "head includes reserved purge-reset positions")
		assert.Greater(t, head, before)

		page, err := member.Read(ctx, projects, before, 100)
		require.NoError(t, err)
		assert.True(t, page.Reset, "a cursor below a purge is a gap")
		assert.GreaterOrEqual(t, page.ResetHead, *purge.PurgeResetAfterEventID)
		assert.Empty(t, page.Occurrences)

		page, err = member.Read(ctx, projects, head, 100)
		require.NoError(t, err)
		assert.False(t, page.Reset, "starting at head never rediscovers a passed purge")
	})
}

func TestMCPEventsAuthorizedScopedReset(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		scoped := f.as(t, f.scoped)
		projects, err := scoped.VisibleProjects(ctx, nil)
		require.NoError(t, err)
		require.Len(t, projects, 1)
		start, err := scoped.Head(ctx, projects)
		require.NoError(t, err)

		// An event inside the subtree is projected for the scoped credential.
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "grandchild", Author: "coordinator",
			Links: []db.InitialLink{{Type: "parent", ToNumber: f.childIssue.ID}}})
		require.NoError(t, err)
		page, err := scoped.Read(ctx, projects, start, 100)
		require.NoError(t, err)
		assert.False(t, page.Reset)
		require.NotEmpty(t, page.Occurrences)

		// A rename of the scoped project can change derived views without naming
		// issues, so a scoped reader resets, as the HTTP poll does.
		_, _, _, err = store.RenameProjectAndEvent(ctx, f.public.ID, "renamed-project", db.BootstrapActor)
		require.NoError(t, err)
		page, err = scoped.Read(ctx, projects, start, 100)
		require.NoError(t, err)
		assert.True(t, page.Reset)
		head, err := scoped.Head(ctx, projects)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, page.ResetHead, head)
	})
}

func TestMCPEventsAuthorizeOccurrence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		for _, tc := range []struct {
			name   string
			who    mcpevents.Principal
			o      mcpevents.Occurrence
			hidden bool
		}{
			{"member sees the team issue", f.member, mcpevents.Occurrence{ProjectUID: f.hidden.UID, IssueUID: f.hiddenIssue.UID}, false},
			{"outsider cannot see the team issue", f.outsider, mcpevents.Occurrence{ProjectUID: f.hidden.UID, IssueUID: f.hiddenIssue.UID}, true},
			{"outsider cannot see the team project", f.outsider, mcpevents.Occurrence{ProjectUID: f.hidden.UID}, true},
			{"outsider cannot see the issue by uid alone", f.outsider, mcpevents.Occurrence{IssueUID: f.hiddenIssue.UID}, true},
			{"outsider sees the public issue", f.outsider, mcpevents.Occurrence{ProjectUID: f.public.UID, IssueUID: f.publicIssue.UID}, false},
			{"unknown issue", f.outsider, mcpevents.Occurrence{IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, true},
			{"unknown project", f.member, mcpevents.Occurrence{ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, true},
			{"scoped sees its subtree", f.scoped, mcpevents.Occurrence{ProjectUID: f.public.UID, IssueUID: f.childIssue.UID}, false},
			{"scoped sees its root", f.scoped, mcpevents.Occurrence{ProjectUID: f.public.UID, IssueUID: f.publicIssue.UID}, false},
			{"scoped cannot see another project", f.scoped, mcpevents.Occurrence{ProjectUID: f.hidden.UID}, true},
			{"scoped cannot see an issue outside the subtree", f.scoped, mcpevents.Occurrence{ProjectUID: f.hidden.UID, IssueUID: f.hiddenIssue.UID}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := f.as(t, tc.who).AuthorizeOccurrence(ctx, tc.o)
				if tc.hidden {
					assert.ErrorIs(t, err, mcpevents.ErrOccurrenceHidden)
				} else {
					assert.NoError(t, err)
				}
			})
		}
		// Moving the issue out of the scoped subtree hides it from the credential.
		sibling, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "sibling", Author: "coordinator"})
		require.NoError(t, err)
		err = f.as(t, f.scoped).AuthorizeOccurrence(ctx, mcpevents.Occurrence{ProjectUID: f.public.UID, IssueUID: sibling.UID})
		assert.ErrorIs(t, err, mcpevents.ErrOccurrenceHidden)

		// Revoking a scoped credential is noticed on the next use.
		scoped := f.as(t, f.scoped)
		_, _, err = store.RevokeAPIToken(ctx, tokenID(t, f.scoped), db.BootstrapActor)
		require.NoError(t, err)
		_, err = scoped.VisibleProjects(ctx, nil)
		assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
	})
}

func tokenID(t *testing.T, p mcpevents.Principal) int64 {
	t.Helper()
	id, err := strconv.ParseInt(p.Subject, 10, 64)
	require.NoError(t, err)
	return id
}

// serviceFixture wires a real Service over the daemon Authority and a TLS
// receiver, so subscriptions are authorized by the daemon's own decisions.
func newDaemonService(t *testing.T, store db.Storage, f *eventsFixture) (*mcpevents.Service, []byte) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Challenge string `json:"challenge"`
		}
		_ = json.UnmarshalRead(r.Body, &body)
		_ = json.MarshalWrite(w, map[string]string{"challenge": body.Challenge})
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	webhook := mcpevents.NewWebhookClient(mcpevents.WebhookOptions{
		Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
		RootCAs: roots,
	})
	keys := mcpevents.NewFileKeyProvider(filepath.Join(t.TempDir(), "mcp-events.key"))
	service, err := mcpevents.NewService(mcpevents.ServiceConfig{Store: store, Keys: keys, Authority: f.authority, Webhook: webhook})
	require.NoError(t, err)
	require.True(t, service.Availability().Enabled, service.Availability().Reason)
	key, err := keys.Load(t.Context())
	require.NoError(t, err)
	return service, key
}

func TestMCPEventsServiceUsesTheDaemonAuthority(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		service, key := newDaemonService(t, store, f)
		ctx := t.Context()
		secret := "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
		request := func(uids ...string) mcpevents.SubscribeRequest {
			args := mcpevents.Arguments{For: "example-user"}
			if len(uids) > 0 {
				args.ProjectUIDs = uids
			}
			return mcpevents.SubscribeRequest{
				Name: mcpevents.EventName, Arguments: args,
				Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/events?route=inbox", Secret: secret},
			}
		}
		all := mcpevents.ForwarderScope{All: true}

		result, err := service.Subscribe(ctx, f.member, all, request())
		require.NoError(t, err)
		head, err := f.as(t, f.member).Head(ctx, nil)
		require.NoError(t, err)
		cursor, err := mcpevents.DecodeCursor(key, result.ID, result.Cursor)
		require.NoError(t, err)
		assert.Equal(t, head, cursor.EventID, "a new subscription starts at the daemon's head")

		// An outsider cannot subscribe to a project policy hides from them.
		_, err = service.Subscribe(ctx, f.outsider, all, request(f.hidden.UID))
		var wire *mcpevents.Error
		require.ErrorAs(t, err, &wire)
		assert.Equal(t, mcpevents.CodeForbidden, wire.Code)
		assert.Equal(t, "scope_removed", wire.Reason)

		// A dynamic subscription for the same outsider is fine and sees no hidden project.
		_, err = service.Subscribe(ctx, f.outsider, all, request())
		require.NoError(t, err)

		// A scoped credential cannot name an issue outside its subtree.
		scopedRequest := request()
		scopedRequest.Arguments.IssueUID = f.hiddenIssue.UID
		_, err = service.Subscribe(ctx, f.scoped, all, scopedRequest)
		require.ErrorAs(t, err, &wire)
		assert.Equal(t, "issue_not_visible", wire.Reason)

		// Revoking the token ends the principal's authority.
		_, _, err = store.RevokeAPIToken(ctx, tokenID(t, f.member), db.BootstrapActor)
		require.NoError(t, err)
		_, err = service.Subscribe(ctx, f.member, all, request())
		require.ErrorAs(t, err, &wire)
		assert.Equal(t, mcpevents.CodeForbidden, wire.Code)
		assert.Equal(t, "principal_revoked", wire.Reason)
		statuses, err := service.Status(ctx, f.member)
		require.NoError(t, err)
		require.Len(t, statuses, 1)
		assert.Equal(t, "ended", statuses[0].State)
		assert.Equal(t, "principal_revoked", statuses[0].EndReason)
		assert.Equal(t, "terminated", statuses[0].PendingControl)
	})
}

func TestMCPEventsMoveIsVisibleToTheDepartureProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		member := f.as(t, f.member)
		mover, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "mover", Author: "coordinator"})
		require.NoError(t, err)
		start, err := member.Head(ctx, nil)
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: mover.ID, FromProjectID: f.public.ID, ToProjectID: f.hidden.ID, IfMatchRev: mover.Revision, Actor: "coordinator"})
		require.NoError(t, err)

		departure := []mcpevents.Project{{UID: f.public.UID, ID: f.public.ID, Name: f.public.Name}}
		result, err := mcpevents.Scan(ctx, member, store.InstanceUID(), mcpevents.Arguments{For: "example-user"}, departure, start, 100)
		require.NoError(t, err)
		require.Len(t, result.Matches, 1, "a subscriber of the departure project learns of the move")
		match := result.Matches[0]
		assert.Equal(t, "visibility", match.Reason)
		assert.Equal(t, f.public.UID, match.ProjectUID)
		assert.Empty(t, match.IssueUID, "the destination project and the issue stay undisclosed")
	})
}

func TestMCPEventsReadSeesAWholeProjectPurge(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		for name, selection := range map[string][]string{
			"dynamic":  nil,
			"explicit": {f.public.UID, f.hidden.UID},
		} {
			t.Run(name, func(t *testing.T) {
				// Each case needs its own project to purge.
				doomed, err := store.CreateProject(ctx, "doomed-"+name)
				require.NoError(t, err)
				_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: doomed.UID, Visibility: "all"}, db.BootstrapActor)
				require.NoError(t, err)
				_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: doomed.ID, Title: "doomed", Author: "coordinator"})
				require.NoError(t, err)
				selected := selection
				if selected != nil {
					selected = append(append([]string(nil), selected...), doomed.UID)
					sortStrings(selected)
				}
				member := f.as(t, f.member)
				before, err := member.VisibleProjects(ctx, selected)
				require.NoError(t, err)
				require.Contains(t, projectUIDs(before), doomed.UID)
				start, err := member.Head(ctx, before)
				require.NoError(t, err)

				_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: doomed.ID, Actor: "admin", Force: true})
				require.NoError(t, err)
				_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: doomed.ID, Actor: "admin"})
				require.NoError(t, err)

				member = f.as(t, f.member)
				after, err := member.VisibleProjects(ctx, selected)
				require.NoError(t, err)
				require.NotContains(t, projectUIDs(after), doomed.UID, "the purged project left the catalog")
				page, err := member.Read(ctx, after, start, 100)
				require.NoError(t, err)
				assert.True(t, page.Reset, "the purge is a gap even though its project is no longer visible")
			})
		}
	})
}

func sortStrings(s []string) { sort.Strings(s) }
