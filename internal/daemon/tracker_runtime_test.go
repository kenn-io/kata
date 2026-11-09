package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/tracker"
)

func TestTrackerRuntimeSnapshotRequiresIndependentServiceGrant(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	member := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectStaticOwner, Subject: store.InstanceUID()}
	service := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "tracker-service"}
	connection := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first", Provider: "notion", Label: "Example account", Notion: &config.NotionSyncConfig{TokenEnv: "EXAMPLE_NOTION_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: member, Actions: []tracker.Action{tracker.ActionDiscover, tracker.ActionBind, tracker.ActionRead}, Sources: []string{"notion:source"}}}}
	mappings := []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: member, Actor: "member"}, {ProjectUID: project.UID, Principal: service, Actor: "tracker-service"}}
	runtime, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{connection}, mappings)
	require.NoError(t, err)
	public, err := runtime.Connections(ctx, project.UID, member)
	require.NoError(t, err)
	require.Len(t, public, 1)
	selection := connection.Descriptor(store.InstanceUID()).Selection
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	_, err = runtime.RouteBinding(ctx, binding)
	require.ErrorIs(t, err, tracker.ErrDenied, "member grant cannot become background authority")
	connection.Generation = "second"
	serviceGrant := connection.Grants[0]
	serviceGrant.Principal = service
	serviceGrant.Actions = []tracker.Action{tracker.ActionRead, tracker.ActionStatus}
	connection.Grants = append(connection.Grants, serviceGrant)
	require.NoError(t, runtime.Reconfigure(ctx, []config.TrackerConnectionConfig{connection}, mappings))
	// Old selection is not silently rebound to the successor generation.
	_, err = runtime.RouteBinding(ctx, binding)
	require.ErrorIs(t, err, tracker.ErrUnavailable)
	// Invalid same-generation edits suspend named use, including discovery.
	connection.Notion = &config.NotionSyncConfig{TokenEnv: "OTHER_EXAMPLE_NOTION_TOKEN"}
	require.ErrorIs(t, runtime.Reconfigure(ctx, []config.TrackerConnectionConfig{connection}, mappings), tracker.ErrGenerationChanged)
	_, err = runtime.Connections(ctx, project.UID, member)
	require.ErrorIs(t, err, tracker.ErrUnavailable)
}

func TestTrackerMappingCannotBroadenEffectiveProjectScope(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	principal := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}
	r, err := NewTrackerRuntime(ctx, store, nil, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: principal, Actor: "member"}})
	require.NoError(t, err)
	snapshot, err := r.current()
	require.NoError(t, err)
	_, err = r.mappedContext(db.WithAuthorizedProjects(ctx, []string{}), snapshot, project.UID, principal)
	require.ErrorIs(t, err, tracker.ErrDenied)
}

// Registry/revocation failures suspend named use while leaving the daemon able
// to serve ordinary project reads and accept a corrected explicit config reload.
func TestTrackerRuntimeInitializationFailureRemainsReloadable(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	runtime, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{{Provider: "unsupported"}}, nil)
	require.Error(t, err)
	require.NotNil(t, runtime)
	_, err = runtime.Connections(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAW", tracker.Principal{})
	require.ErrorIs(t, err, tracker.ErrUnavailable)
	require.NoError(t, runtime.Reconfigure(ctx, nil, nil))
	_, err = store.CreateProject(ctx, "ordinary-project")
	require.NoError(t, err)
}

func TestTrackerMappingBindsAccountableActorWithoutOwnerBypass(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	principal := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "service"}
	runtime, err := NewTrackerRuntime(ctx, store, nil, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: principal, Actor: "accountable-service"}})
	require.NoError(t, err)
	snapshot, err := runtime.current()
	require.NoError(t, err)
	ownerCtx := db.WithOwnerRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID()})
	mapped, err := runtime.mappedContext(ownerCtx, snapshot, project.UID, principal)
	require.NoError(t, err)
	signer, actor, ok := db.RootAttributionFromContext(mapped)
	require.True(t, ok)
	require.Equal(t, "accountable-service", actor)
	require.Equal(t, store.InstanceUID(), signer.AuthorityUID)
	require.False(t, db.RootAttributionOwner(mapped))
}

// Production supplies the runtime signer even when no incoming request has
// attribution. The mapped service actor must be retained, and a foreign signer
// cannot substitute another root authority (GLM48138 source finding).
func TestTrackerConfiguredSignerAttributesPlainContextAndRejectsForeignRoot(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	p := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "tracker-service"}
	for _, authority := range []string{store.InstanceUID(), "01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		r, err := NewTrackerRuntime(ctx, store, nil, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: p, Actor: "mapped-service"}}, NewRootSigning(db.RootAttributionSigner{AuthorityUID: authority}))
		require.NoError(t, err)
		snapshot, err := r.current()
		require.NoError(t, err)
		mapped, err := r.mappedContext(ctx, snapshot, project.UID, p)
		if authority != store.InstanceUID() {
			require.ErrorIs(t, err, tracker.ErrUnavailable)
			continue
		}
		require.NoError(t, err)
		signer, actor, ok := db.RootAttributionFromContext(mapped)
		require.True(t, ok)
		require.Equal(t, store.InstanceUID(), signer.AuthorityUID)
		require.Equal(t, "mapped-service", actor)
		require.False(t, db.RootAttributionOwner(mapped))
	}
}

// An owner-local actor mapping is part of effective connection authority. A
// policy reload cannot silently reuse the generation after reassigning it.
func TestTrackerMappingAuthorityChangeNeedsFreshConnectionGeneration(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	p := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "tracker-service"}
	c := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first", Provider: "notion", Label: "Example account", Notion: &config.NotionSyncConfig{TokenEnv: "EXAMPLE_NOTION_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: p, Actions: []tracker.Action{tracker.ActionRead}, Sources: []string{"notion:source"}}}}
	maps := []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: p, Actor: "original-service"}}
	r, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{c}, maps)
	require.NoError(t, err)
	maps[0].Actor = "replacement-service"
	require.ErrorIs(t, r.Reconfigure(ctx, []config.TrackerConnectionConfig{c}, maps), tracker.ErrGenerationChanged)
	_, err = r.current()
	require.ErrorIs(t, err, tracker.ErrUnavailable)
	c.Generation = "second"
	require.NoError(t, r.Reconfigure(ctx, []config.TrackerConnectionConfig{c}, maps))
}

// Contract: a captured remote typed-method guard checks current root policy
// locally; the actual owner method checks owner grants. Authority-only checks
// such as two-way bind status permission still require their explicit RPC.
func TestTrackerRemoteTypedGuardDoesNotQueueRedundantAuthorization(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "method-guard.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "method-project")
	require.NoError(t, err)
	p := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "sync"}
	r, err := NewTrackerRuntime(ctx, store, nil, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: p, Actor: "root-service"}})
	require.NoError(t, err)
	snapshot, err := r.current()
	require.NoError(t, err)
	selection := tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", Generation: "first"}
	run := &trackerRemoteRun{runtime: r, snapshot: snapshot, project: project.UID, principal: p, descriptor: tracker.PublicDescriptor{Selection: selection, Provider: "notion", Label: "Example account"}}
	authority := trackerRunAuthority{remote: run, root: store.InstanceUID(), project: project.UID, principal: p, source: "notion:source", incarnation: snapshot.incarnation, action: tracker.ActionRead}
	ctx = context.WithValue(tracker.WithRunSelection(ctx, selection), trackerRunAuthorityKey{}, authority)
	guard, err := r.sessionGuard(ctx, "notion")
	require.NoError(t, err)
	_, err = guard.authorize(ctx)
	require.NoError(t, err)
	require.Empty(t, r.workerBroker.calls, "guard alone must not spend another owner RPC")
	_, err = r.selectedConnection(ctx, "notion", tracker.ActionStatus)
	require.ErrorIs(t, err, tracker.ErrUnavailable, "authority-only proof still needs pinned root/provider RPC")
	r.Suspend()
	_, err = guard.authorize(ctx)
	require.ErrorIs(t, err, tracker.ErrUnavailable)
}
