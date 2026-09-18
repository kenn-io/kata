package daemon

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	katauid "go.kenn.io/kata/internal/uid"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestBoundSpokeClaimPrincipalPreservesLocalOwnerIdentity(t *testing.T) {
	binding := db.FederationBinding{Role: db.FederationRoleSpoke, Actor: "spoke-actor"}
	first := boundSpokeClaimPrincipal(binding, db.ClaimPrincipal{
		Holder: hostClaimHolder("user-one"), ClientKind: "cli", AuthenticatedHost: true,
	})
	second := boundSpokeClaimPrincipal(binding, db.ClaimPrincipal{
		Holder: hostClaimHolder("user-two"), ClientKind: "cli", AuthenticatedHost: true,
	})

	assert.Equal(t, "spoke-actor", first.Holder)
	assert.Equal(t, "spoke-actor", second.Holder)
	assert.NotEqual(t, first.ClientKind, second.ClientKind)
}

func TestBoundSpokeClaimPrincipalKeepsLegacyClientIdentity(t *testing.T) {
	binding := db.FederationBinding{Role: db.FederationRoleSpoke, Actor: "spoke-actor"}
	for _, holder := range []string{"local-worker", "host:worker"} {
		legacy := boundSpokeClaimPrincipal(binding, db.ClaimPrincipal{
			Holder: holder, ClientKind: "cli",
		})

		assert.Equal(t, "spoke-actor", legacy.Holder)
		assert.Equal(t, "cli", legacy.ClientKind)
	}
}

func TestNewClaimHubClientHonorsTrustedPrivateNetwork(t *testing.T) {
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")

	client, err := newClaimHubClient(context.Background(), "http://100.64.0.5:7787", "enrollment-token", false)

	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, "http://100.64.0.5:7787", client.baseURL)
}

func TestNewClaimHubClientHonorsAllowInsecureHostnameOptIn(t *testing.T) {
	client, err := newClaimHubClient(context.Background(), "http://tailnet-hub.internal:7787", "enrollment-token", true)

	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, "http://tailnet-hub.internal:7787", client.baseURL)
}

func TestClaimHubClientNormalizesDeprecatedClaimFields(t *testing.T) {
	legacyClaim := &api.IssueClaimOut{
		ClaimUID: "01HZNQ7VFPK1XGD8R5MABCD4EF",
		IssueUID: "01HZNQ7VFPK1XGD8R5MABCD4EG",
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(api.ClaimStatusBody{Held: true, Claim: legacyClaim})
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(api.ClaimActionResponseBody{Granted: true, Claim: legacyClaim})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(hub.Close)

	client, err := newClaimHubClient(context.Background(), hub.URL, "enrollment-token", false)
	require.NoError(t, err)

	actions := []struct {
		name string
		call func() (api.ClaimActionResponseBody, error)
	}{
		{"acquire", func() (api.ClaimActionResponseBody, error) {
			return client.AcquireClaim(context.Background(), 42, "ABC", api.ClaimActionBody{})
		}},
		{"renew", func() (api.ClaimActionResponseBody, error) {
			return client.RenewClaim(context.Background(), 42, "ABC", api.ClaimActionBody{})
		}},
		{"release", func() (api.ClaimActionResponseBody, error) {
			return client.ReleaseClaim(context.Background(), 42, "ABC", api.ClaimActionBody{})
		}},
	}
	for _, action := range actions {
		t.Run(action.name, func(t *testing.T) {
			body, err := action.call()
			require.NoError(t, err)
			assert.Equal(t, legacyClaim, body.Lease)
			assert.Equal(t, legacyClaim, body.Claim)
		})
	}

	status, err := client.ClaimStatus(context.Background(), 42, "ABC")
	require.NoError(t, err)
	assert.Equal(t, legacyClaim, status.Lease)
	assert.Equal(t, legacyClaim, status.Claim)
}

func TestClaimHubClientUsesUnixRuntimeForKataInvalid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets unsupported on windows")
	}
	tmp, err := os.MkdirTemp("/tmp", "kata-claim-unix-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	t.Setenv("TMPDIR", tmp)
	ns, err := NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	sock := filepath.Join(ns.SocketDir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var gotPath string
	var gotAuth string
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/ping":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":      true,
					"service": "kata",
					"version": "test",
					"pid":     os.Getpid(),
				})
			case "/api/v1/projects/42/issues/ABC/lease/actions/acquire":
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				_ = json.NewEncoder(w).Encode(api.ClaimActionResponseBody{Granted: true})
			default:
				http.NotFound(w, r)
			}
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   "unix",
		Address:   sock,
		Metadata:  map[string]string{"db_path": filepath.Join(tmp, "kata.db")},
		StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	client, err := newClaimHubClient(context.Background(), "http://kata.invalid", "enrollment-token", false)
	require.NoError(t, err)
	_, err = client.AcquireClaim(context.Background(), 42, "ABC", api.ClaimActionBody{})

	require.NoError(t, err)
	assert.Equal(t, "/api/v1/projects/42/issues/ABC/lease/actions/acquire", gotPath)
	assert.Equal(t, "Bearer enrollment-token", gotAuth)
}

// A forwarded no-op must uphold the claim response contract in the Go
// response object itself: events is a required array, so the construction
// path normalizes a nil slice (a hub body with no events) to the empty
// array rather than relying on the encoder's nil-slice rendering.
func TestApplyForwardedAssignmentClaimNoOpNormalizesEmptyEvents(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "claim target",
		Author:    "tester",
	})
	require.NoError(t, err)

	// A hub no-op carries no events, so forwarded Events decodes nil just
	// like an absent or null JSON array would.
	response, err := applyForwardedAssignmentClaim(ctx, ServerConfig{DB: store}, project.ID, issue.UID, api.ClaimResponseBody{Changed: false})
	require.NoError(t, err)
	require.False(t, response.Body.Changed)
	require.Nil(t, response.Body.Event)
	require.NotNil(t, response.Body.Events, "events is a required array: nil must be normalized at the response boundary")
	require.Empty(t, response.Body.Events)

	// encoding/json v1 renders a nil slice as null, so the response object
	// must carry the empty array itself even outside the daemon's JSON v2
	// writer.
	raw, err := json.Marshal(response.Body)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"events":[]`)
}

// forwardedHubWireEvent builds a hub-side event as the claim response would
// carry it across federation: full payload, hub instance identity, and a valid
// portable content hash so the spoke's insert accepts it.
func forwardedHubWireEvent(
	t *testing.T,
	project db.Project,
	issueUID string,
	eventType, actor, originInstanceUID string,
	physicalMS int64,
	payload string,
) db.Event {
	t.Helper()
	eventUID, err := katauid.New()
	require.NoError(t, err)
	event := db.Event{
		UID: eventUID, OriginInstanceUID: originInstanceUID,
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueUID: &issueUID, Type: eventType, Actor: actor,
		HLCPhysicalMS: physicalMS, HLCCounter: 2, Payload: payload,
		CreatedAt: time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	}
	event.ContentHash, err = db.EventContentHash(db.EventHashInput{
		UID: event.UID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter,
		CreatedAt:  event.CreatedAt.UTC().Format(db.EventTimestampFormat),
		Payload:    jsontext.Value(event.Payload),
	})
	require.NoError(t, err)
	return event
}

// An issue-scoped caller reading a forwarded assignment response must see the
// same typed safe projection every other scoped response applies: out-of-scope
// events drop, hub infrastructure identity (origin instance UID and content
// hash) redacts. The raw events still publish so mirror SSE subscribers and
// hooks keep their durable form; shipped middleware already rejects scoped
// tokens on spokes, so this seam test exercises the response projection only.
func TestApplyForwardedAssignmentClaimProjectsScopedResponseAndPublishesRaw(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "scoped claim target", Author: "tester",
	})
	require.NoError(t, err)
	sibling, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "outside subtree", Author: "tester",
	})
	require.NoError(t, err)
	// The seam runs on a spoke mirror: materialization needs the binding row.
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID,
		ReplayHorizonEventID: 1, Enabled: true,
	})
	require.NoError(t, err)

	hubInstanceUID := "01HZNQ7VFPK1XGD8R5MABCD4EH"
	assigned := forwardedHubWireEvent(t, project, root.UID, "issue.assigned", "tester", hubInstanceUID, 500,
		`{"owner":"tester","assignment_expires_on":"2026-05-23T13:00:00.000Z","updated_at":"2026-05-23T12:05:00.000Z"}`)
	siblingUpdated := forwardedHubWireEvent(t, project, sibling.UID, "issue.updated", "tester", hubInstanceUID, 501,
		`{"title":"sibling title","updated_at":"2026-05-23T12:05:01.000Z"}`)
	forwarded := api.ClaimResponseBody{
		Changed: true,
		Events:  []db.Event{assigned, siblingUpdated},
	}

	broadcaster := NewEventBroadcaster()
	sub := broadcaster.Subscribe(SubFilter{ProjectID: project.ID})
	defer sub.Unsub()

	scopedCtx := WithPrincipal(ctx, Principal{
		Kind: PrincipalDBToken,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
	})
	response, err := applyForwardedAssignmentClaim(scopedCtx, ServerConfig{DB: store, Broadcaster: broadcaster},
		project.ID, root.UID, forwarded)
	require.NoError(t, err)

	// The out-of-subtree event drops and the surviving event loses hub
	// infrastructure identity.
	require.Len(t, response.Body.Events, 1)
	projected := response.Body.Events[0]
	assert.Equal(t, assigned.UID, projected.UID)
	assert.Equal(t, "issue.assigned", projected.Type)
	assert.Empty(t, projected.OriginInstanceUID, "scoped responses must not carry the hub instance UID")
	assert.Empty(t, projected.ContentHash, "scoped responses must not carry portable event hashes")
	require.NotNil(t, response.Body.Event)
	assert.Equal(t, projected.UID, response.Body.Event.UID)

	// Publishing remains raw: mirror subscribers receive every inserted event
	// with its durable payload and hub identity intact.
	first := receiveInternalMsg(t, sub.Ch, "raw assigned broadcast")
	require.NotNil(t, first.Event)
	assert.Equal(t, assigned.UID, first.Event.UID)
	assert.Equal(t, hubInstanceUID, first.Event.OriginInstanceUID)
	assert.Equal(t, assigned.Payload, first.Event.Payload)
	second := receiveInternalMsg(t, sub.Ch, "raw sibling broadcast")
	require.NotNil(t, second.Event)
	assert.Equal(t, siblingUpdated.UID, second.Event.UID)
	assert.Equal(t, hubInstanceUID, second.Event.OriginInstanceUID)

	// An unscoped caller keeps the raw response shape: both events, hub
	// identity intact.
	unscoped, err := applyForwardedAssignmentClaim(ctx, ServerConfig{DB: store, Broadcaster: NewEventBroadcaster()},
		project.ID, root.UID, forwarded)
	require.NoError(t, err)
	require.Len(t, unscoped.Body.Events, 2)
	assert.Equal(t, hubInstanceUID, unscoped.Body.Events[0].OriginInstanceUID)
	assert.Equal(t, hubInstanceUID, unscoped.Body.Events[1].OriginInstanceUID)
}

func receiveInternalMsg(t *testing.T, ch <-chan StreamMsg, label string) StreamMsg {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: did not receive within timeout", label)
		return StreamMsg{}
	}
}
