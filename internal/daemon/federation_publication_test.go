package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

const publicationPath = "/api/v1/federation/publications"

func publicationEnvelope(t *testing.T, target db.Storage) []byte {
	t.Helper()
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	p, _, err := source.CreateProjectAndEvent(t.Context(), "source-project", "original-author")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Original issue", Author: "original-author"})
	require.NoError(t, err)
	document, err := jsonl.CaptureFederationPublication(t.Context(), source, p.ID, db.FederationPublication{
		OperationUID: "01K00000000000000000000005", SourceInstanceUID: source.InstanceUID(),
		ProjectUID: p.UID, ProjectName: p.Name, TargetInstanceUID: target.InstanceUID(),
	})
	require.NoError(t, err)
	raw, err := jsonl.EncodeFederationPublication(document)
	require.NoError(t, err)
	return raw
}

func publicationRequest(t *testing.T, handler http.Handler, token string, envelope []byte, preflight bool, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"preflight": preflight, "envelope": jsontext.Value(envelope)}
	maps.Copy(body, extra)
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, publicationPath, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// The publication contract requires ordinary account authority even for preview;
// broad owner/anonymous authority must never turn this into an owner replay API.
func TestPublicationAccountAuthorization(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		envelope := publicationEnvelope(t, store)
		scoped, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
			PlaintextToken: "scoped-publication-token", Actor: "member", AdminActor: "admin",
			Scope:     &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: f.visible.UID},
			ExpiresAt: new(time.Now().Add(time.Hour)),
		})
		require.NoError(t, err)
		_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "revoked-publication-token", Actor: "member", AdminActor: "admin"})
		require.NoError(t, err)
		revoked, err := store.ResolveAPIToken(t.Context(), "revoked-publication-token")
		require.NoError(t, err)
		_, _, err = store.RevokeAPIToken(t.Context(), revoked.ID, "admin")
		require.NoError(t, err)
		require.NotZero(t, scoped.ID)
		before, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		for _, tc := range []struct {
			token  string
			status int
		}{
			{"bootstrap-test-token", 403}, {"scoped-publication-token", 403},
			{"revoked-publication-token", 403}, {"", 401},
		} {
			for _, preview := range []bool{true, false} {
				w := publicationRequest(t, f.handler, tc.token, envelope, preview, nil)
				require.Equal(t, tc.status, w.Code, w.Body.String())
			}
		}
		w := publicationRequest(t, f.handler, "member-test-token", envelope, true, nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		var preview map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &preview))
		require.Equal(t, "ready", preview["status"])
		require.Equal(t, "all", preview["visibility"])
		require.Equal(t, "member", preview["account"])
		require.NotContains(t, preview, "project_id")
		after, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
		_, err = store.ProjectByName(t.Context(), "source-project")
		require.ErrorIs(t, err, db.ErrNotFound)
		for _, restriction := range []map[string]any{{"visibility": "teams"}, {"policy": map[string]any{}}, {"team_uids": []string{"team"}}} {
			w = publicationRequest(t, f.handler, "member-test-token", envelope, false, restriction)
			require.Equal(t, 400, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "federation_publication_invalid")
		}
		w = publicationRequest(t, f.handler, "member-test-token", envelope, false, nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		var published struct {
			Status     string `json:"status"`
			Visibility string `json:"visibility"`
			Account    string `json:"account"`
			ProjectID  int64  `json:"project_id"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &published))
		require.Equal(t, "published", published.Status)
		policy, err := store.ProjectAccessPolicy(t.Context(), mustPublicationProject(t, store).UID)
		require.NoError(t, err)
		require.Equal(t, policy.Visibility, published.Visibility)
		require.Positive(t, published.ProjectID)
		request := httptest.NewRequest(http.MethodGet, "/api/v1/instance", nil)
		request.Header.Set("Authorization", "Bearer member-test-token")
		r := httptest.NewRecorder()
		f.handler.ServeHTTP(r, request)
		require.Contains(t, r.Body.String(), `"relay_publication_version":1`)
	})
}

func mustPublicationProject(t *testing.T, store db.Storage) db.Project {
	t.Helper()
	p, err := store.ProjectByName(t.Context(), "source-project")
	require.NoError(t, err)
	return p
}

type publicationHooks struct{ events []db.Event }

func (h *publicationHooks) Enqueue(e db.Event) { h.events = append(h.events, e) }

type publicationResponseFailure struct {
	db.Storage
	fail   bool
	before func()
}

func (s *publicationResponseFailure) PublishFederationProject(ctx context.Context, p db.FederationPublication, actor string) (db.FederationPublicationResult, error) {
	if s.before != nil {
		s.before()
	}
	return s.Storage.(db.FederationPublicationStore).PublishFederationProject(ctx, p, actor)
}
func (s *publicationResponseFailure) ProjectAccessPolicy(ctx context.Context, uid string) (db.ProjectAccessPolicy, error) {
	if s.fail {
		s.fail = false
		return db.ProjectAccessPolicy{}, errors.New("private diagnostic: /tmp/example.secret")
	}
	return s.Storage.ProjectAccessPolicy(ctx, uid)
}

func TestPublicationResponseLossAndCommitFence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "account-token", Actor: "publisher", AdminActor: "admin"})
		require.NoError(t, err)
		wrapped := &publicationResponseFailure{Storage: store, fail: true}
		hooks := &publicationHooks{}
		server := daemon.NewServer(daemon.ServerConfig{DB: wrapped, Hooks: hooks, Auth: config.AuthConfig{Token: "bootstrap", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		envelope := publicationEnvelope(t, store)
		w := publicationRequest(t, server.Handler(), "account-token", envelope, false, nil)
		require.Equal(t, 500, w.Code, w.Body.String())
		require.NotContains(t, w.Body.String(), "example.secret")
		p := mustPublicationProject(t, store)
		require.Len(t, hooks.events, 1, "catalog event precedes response-only reads")
		require.Equal(t, "publisher", hooks.events[0].Actor)
		_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Later hub edit", Author: "publisher"})
		require.NoError(t, err)
		w = publicationRequest(t, server.Handler(), "account-token", envelope, false, nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"replayed":true`)
		require.Len(t, hooks.events, 1)
		// An account revoked after HTTP admission cannot replay the completion marker.
		wrapped.before = func() { _, _, err := store.RevokeAPIToken(t.Context(), token.ID, "admin"); require.NoError(t, err) }
		w = publicationRequest(t, server.Handler(), "account-token", envelope, false, nil)
		require.Equal(t, 401, w.Code, w.Body.String())
		require.Len(t, hooks.events, 1)
	})
}

type publicationCapacityStore struct{ *publicationResponseFailure }

func (publicationCapacityStore) FederationPublicationCapacity() int64 { return 1 }

func TestPublicationCapacityAndUnsupportedStorage(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "account-token", Actor: "publisher", AdminActor: "admin"})
	require.NoError(t, err)
	envelope := publicationEnvelope(t, store)
	for _, tc := range []struct {
		name   string
		store  db.Storage
		status int
		code   string
	}{
		{"capacity", publicationCapacityStore{&publicationResponseFailure{Storage: store}}, 413, "federation_publication_too_large"},
		{"unsupported", struct{ db.Storage }{store}, 409, "federation_publication_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := daemon.NewServer(daemon.ServerConfig{DB: tc.store, Auth: config.AuthConfig{Token: "bootstrap", RequireTokenIdentity: true}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			before, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			for _, preview := range []bool{true, false} {
				w := publicationRequest(t, server.Handler(), "account-token", envelope, preview, nil)
				require.Equal(t, tc.status, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), tc.code)
			}
			after, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestPublicationRevokedBeforeFirstCommit(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		token, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "account-token", Actor: "publisher", AdminActor: "admin"})
		require.NoError(t, err)
		wrapped := &publicationResponseFailure{Storage: store}
		wrapped.before = func() { _, _, err := store.RevokeAPIToken(t.Context(), token.ID, "admin"); require.NoError(t, err) }
		hooks := &publicationHooks{}
		server := daemon.NewServer(daemon.ServerConfig{DB: wrapped, Hooks: hooks, Auth: config.AuthConfig{Token: "bootstrap", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		w := publicationRequest(t, server.Handler(), "account-token", publicationEnvelope(t, store), false, nil)
		require.Equal(t, 401, w.Code, w.Body.String())
		_, err = store.ProjectByName(t.Context(), "source-project")
		require.ErrorIs(t, err, db.ErrNotFound)
		require.Empty(t, hooks.events)
	})
}

func TestPublicationRejectsHiddenCollisions(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		envelope := publicationEnvelope(t, store)
		p, err := store.CreateProject(t.Context(), "source-project")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: p.UID, Visibility: "teams"}, "admin")
		require.NoError(t, err)
		for _, preview := range []bool{true, false} {
			w := publicationRequest(t, f.handler, "nonmember-test-token", envelope, preview, nil)
			require.Equal(t, 409, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "federation_publication_conflict")
			require.NotContains(t, w.Body.String(), p.UID)
			require.NotContains(t, w.Body.String(), p.Name)
		}
	})
}

type publicationHostDenied struct{ request *daemon.HostAccessRequest }

func (h publicationHostDenied) Authorize(_ context.Context, request daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	*h.request = request
	return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
}

func TestPublicationReadOnlyAndHostDenial(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	envelope := publicationEnvelope(t, store)
	readonly := daemon.NewServer(daemon.ServerConfig{DB: store, InsecureReadonly: true})
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	for _, preview := range []bool{true, false} {
		w := publicationRequest(t, readonly.Handler(), "", envelope, preview, nil)
		require.Equal(t, 401, w.Code, w.Body.String())
	}
	var request daemon.HostAccessRequest
	host := daemon.NewServer(daemon.ServerConfig{DB: store, HostAccess: publicationHostDenied{request: &request}})
	t.Cleanup(func() { require.NoError(t, host.Close()) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host.Handler().ServeHTTP(w, r.WithContext(daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Actor: "publisher", Subject: "publisher"})))
	})
	w := publicationRequest(t, handler, "", envelope, false, nil)
	require.Equal(t, 404, w.Code, w.Body.String())
	require.Equal(t, "project_administration", string(request.Operation.Policy.Kind))
	require.Equal(t, "manage", string(request.Operation.Policy.Capability))
	_, err = store.ProjectByName(t.Context(), "source-project")
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestPublicationPreflightRequiresBoolean(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		envelope := publicationEnvelope(t, store)
		for _, value := range []any{nil, "true", 1} {
			w := publicationRequest(t, f.handler, "member-test-token", envelope, true, map[string]any{"preflight": value})
			require.Equal(t, 400, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "federation_publication_invalid")
		}
		_, err := store.ProjectByName(t.Context(), "source-project")
		require.ErrorIs(t, err, db.ErrNotFound)
	})
}

func TestPublicationReplayRechecksProjectAccess(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		envelope := publicationEnvelope(t, store)
		w := publicationRequest(t, f.handler, "member-test-token", envelope, false, nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		p := mustPublicationProject(t, store)
		_, _, _, err := store.RenameProjectAndEvent(t.Context(), p.ID, "hidden-project", "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: p.UID, Visibility: "teams"}, "admin")
		require.NoError(t, err)
		before, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		w = publicationRequest(t, f.handler, "member-test-token", envelope, false, nil)
		require.Equal(t, 404, w.Code, w.Body.String())
		require.NotContains(t, w.Body.String(), "hidden-project")
		require.NotContains(t, w.Body.String(), p.UID)
		after, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
	})
}

func TestPublicationRequestByteLimit(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := newProjectAccessFixture(t, store)
	before, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	// A maximum-size envelope still needs room for the exact JSON wrapper.
	// Invalid JSON at that boundary must reach validation, rather than being
	// rejected by the body reader before a valid envelope could be decoded.
	maximum := db.MaxRelayEnvelopeBytes + len(`{"preflight":false,"envelope":}`)
	for _, tc := range []struct{ size, status int }{{maximum, 400}, {maximum + 1, 413}} {
		req := httptest.NewRequest(http.MethodPost, publicationPath, bytes.NewReader(bytes.Repeat([]byte(" "), tc.size)))
		req.Header.Set("Authorization", "Bearer member-test-token")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		require.Equal(t, tc.status, w.Code, w.Body.String())
	}
	after, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}
