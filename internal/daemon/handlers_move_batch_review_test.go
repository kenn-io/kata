package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestClaimClientCache_LocalHubRecovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets unsupported on windows")
	}
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	home := os.Getenv("KATA_HOME")
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	binding := db.FederationBinding{ProjectID: project.ID, HubURL: "http://kata.invalid", HubProjectID: 42}
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: binding.HubURL, HubProjectID: 42, Token: "synthetic-token"}))
	cache := &claimClientCache{}
	cfg := ServerConfig{DB: store}
	first, _, err := cache.get(t.Context(), cfg, binding)
	require.NoError(t, err)
	_, err = first.ClaimStatus(t.Context(), 42, "first")
	require.Error(t, err)

	sock := filepath.Join(ns.SocketDir, "daemon.sock")
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ping":
			_ = json.MarshalWrite(w, map[string]any{"ok": true, "service": "kata", "version": "test", "pid": os.Getpid()})
		case "/api/v1/projects/42/issues/second/lease":
			_ = json.MarshalWrite(w, api.ClaimStatusBody{})
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.WithoutCancel(t.Context())) })
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "unix", Address: sock, Metadata: map[string]string{"db_path": filepath.Join(home, "kata.db")}, StartedAt: time.Now().UTC()})
	require.NoError(t, err)
	second, _, err := cache.get(t.Context(), cfg, binding)
	require.NoError(t, err)
	_, err = second.ClaimStatus(t.Context(), 42, "second")
	require.NoError(t, err)
}

type batchTargetFailureStore struct {
	db.Storage
	target string
	failed bool
}

func (s *batchTargetFailureStore) ProjectByUID(ctx context.Context, uid string) (db.Project, error) {
	if uid == s.target && !s.failed {
		s.failed = true
		return db.Project{}, errors.New("target temporarily unavailable")
	}
	return s.Storage.ProjectByUID(ctx, uid)
}

func TestMoveIssuesBatch_TargetFailureIsPerItem(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, first := createClaimGateHelperIssue(t, store)
	second, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "independent", Author: "agent"})
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Items = []api.MoveIssuesItem{{Ref: first.UID, ToProjectUID: target.UID}, {Ref: second.UID, ToProjectUID: target.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: &batchTargetFailureStore{Storage: store, target: target.UID}})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "refused", out.Body.Results[0].Result)
	require.Equal(t, "moved", out.Body.Results[1].Result)
	got, err := store.IssueByID(t.Context(), second.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
}

// Batch defaults must leave a refused child behind when its parent moves.
func TestMoveIssuesBatch_SelectedChildRevision(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "revision-target")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	owner := "new-owner"
	changed, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: child.ID, Owner: &owner, Actor: "agent"})
	require.NoError(t, err)
	require.Greater(t, changed.Revision, child.Revision)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Comment = "agent", "routed"
	req.Body.Items = []api.MoveIssuesItem{{Ref: child.UID, ExpectedRevision: child.Revision, ToProjectUID: target.UID}, {Ref: root.UID, ToProjectUID: target.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, out.Body.Results, 2)
	require.Equal(t, "revision_conflict", out.Body.Results[0].Code)
	require.Equal(t, "moved", out.Body.Results[1].Result)
	for _, before := range []db.Issue{root, child} {
		got, err := store.IssueByID(t.Context(), before.ID)
		require.NoError(t, err)
		want := source.ID
		if before.ID == root.ID {
			want = target.ID
		}
		require.Equal(t, want, got.ProjectID)
	}
}

func TestMoveBatchRefusal_FinalizationFailurePrecedesDenial(t *testing.T) {
	var result api.MoveIssuesResult
	moveBatchRefusal(&result, errors.Join(api.NewError(404, "not_found", "not found", "", nil), db.ErrTransactionFinalizationFailed))
	require.Equal(t, "refused", result.Result)
	require.Equal(t, "access_unavailable", result.Code)
}

func TestClaimClientCache_ReuseRecoveryAndReplacement(t *testing.T) {
	t.Run("transient credential failure", func(t *testing.T) {
		store := openClaimGateHelperDB(t)
		project, _ := createClaimGateHelperIssue(t, store)
		creds := &batchCountingCredentials{FederationCredentialStore: config.DefaultFederationCredentialStore(), failFirst: true}
		cache := &claimClientCache{}
		binding := db.FederationBinding{ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 1}
		require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: binding.HubURL, HubProjectID: binding.HubProjectID, Token: "synthetic-token"}))
		cfg := ServerConfig{DB: store, FederationCredentials: creds}
		failed, _, err := cache.get(t.Context(), cfg, binding)
		require.Error(t, err)
		require.Nil(t, failed)
		client, credential, err := cache.get(t.Context(), cfg, binding)
		require.NoError(t, err)
		require.NotNil(t, client)
		require.Equal(t, "synthetic-token", credential.Token)
		again, _, err := cache.get(t.Context(), cfg, binding)
		require.NoError(t, err)
		require.Same(t, client, again)
		require.EqualValues(t, 2, creds.reads.Load())
	})

	cases := []struct {
		name    string
		replace func(*db.FederationBinding)
	}{
		{"endpoint", func(binding *db.FederationBinding) { binding.HubURL = "https://replacement-hub.example" }},
		{"hub project", func(binding *db.FederationBinding) { binding.HubProjectID = 2 }},
		{"transport opt-in", func(binding *db.FederationBinding) { binding.AllowInsecure = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			project, _ := createClaimGateHelperIssue(t, store)
			creds := &batchCountingCredentials{FederationCredentialStore: config.DefaultFederationCredentialStore()}
			cache := &claimClientCache{}
			binding := db.FederationBinding{ProjectID: project.ID, HubURL: "https://old-hub.example", HubProjectID: 1}
			require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: binding.HubURL, HubProjectID: 1, Token: "old-token"}))
			cfg := ServerConfig{DB: store, FederationCredentials: creds}
			first, credential, err := cache.get(t.Context(), cfg, binding)
			require.NoError(t, err)
			require.Equal(t, "old-token", credential.Token)
			again, _, err := cache.get(t.Context(), cfg, binding)
			require.NoError(t, err)
			require.Same(t, first, again)
			require.EqualValues(t, 1, creds.reads.Load())
			tc.replace(&binding)
			require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: binding.HubURL, HubProjectID: binding.HubProjectID, Token: "replacement-token"}))
			replacement, credential, err := cache.get(t.Context(), cfg, binding)
			require.NoError(t, err)
			require.NotSame(t, first, replacement)
			require.Equal(t, binding.HubURL, credential.HubURL)
			require.Equal(t, binding.HubProjectID, credential.HubProjectID)
			require.Equal(t, binding.AllowInsecure, credential.AllowInsecure)
			require.Equal(t, "replacement-token", credential.Token)
			again, _, err = cache.get(t.Context(), cfg, binding)
			require.NoError(t, err)
			require.Same(t, replacement, again)
			require.EqualValues(t, 2, creds.reads.Load())
		})
	}
}
