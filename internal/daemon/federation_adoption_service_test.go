package daemon_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/federationpublication"
)

type adoptionFixture struct {
	hub     bridgeHubFixture
	store   db.Storage
	project db.Project
	home    string
	local   projectAccessFixture
	signer  db.RootAttributionSigner
}

func newAdoptionFixture(t *testing.T, intercept func(http.ResponseWriter, *http.Request, http.Handler)) adoptionFixture {
	t.Helper()
	home, err := os.MkdirTemp("", "kra-adoption-workflow-*")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(home)) })
	t.Setenv("KATA_HOME", home)
	rootStore := openReplicaServiceStore(t)
	root := newProjectAccessFixture(t, rootStore)
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := db.RootAttributionSigner{AuthorityUID: rootStore.InstanceUID(), PrivateKey: key}
	server := daemon.NewServer(daemon.ServerConfig{DB: rootStore, RootSigning: daemon.NewRootSigning(signer), Broadcaster: root.broadcaster, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	root.server = httptest.NewServer(server.Handler())
	t.Cleanup(root.server.Close)
	f := adoptionFixture{hub: bridgeHubFixture{store: rootStore, root: root}, store: openReplicaServiceStore(t), home: home, signer: signer}
	f.project, _, err = f.store.CreateProjectAndEvent(t.Context(), "shared-replica", "original-author")
	require.NoError(t, err)
	_, _, err = f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Retained source issue", Author: "original-author"})
	require.NoError(t, err)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forward := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req, err := http.NewRequestWithContext(r.Context(), r.Method, f.hub.root.server.URL+r.URL.RequestURI(), r.Body) //nolint:gosec // The URL is the fixture's loopback-only httptest server.
			if err != nil {
				t.Error(err)
				return
			}
			req.Header = r.Header.Clone()
			response, err := f.hub.root.server.Client().Do(req) //nolint:gosec // The request targets the fixture's loopback-only httptest server.
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer func() { _ = response.Body.Close() }()
			for key, values := range response.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
		})
		if intercept != nil {
			intercept(w, r, forward)
			return
		}
		forward.ServeHTTP(w, r)
	})
	remote := httptest.NewServer(upstream)
	t.Cleanup(remote.Close)
	f.local = newAdoptionLocalDaemon(t, f.store, f.hub.catalog(remote.URL))
	return f
}

func newAdoptionLocalDaemon(t *testing.T, store db.Storage, catalog []config.CatalogDaemonConfig) projectAccessFixture {
	return newAdoptionLocalDaemonUsing(t, store, catalog, config.DefaultFederationCredentialStore())
}

func newAdoptionLocalDaemonUsing(t *testing.T, store db.Storage, catalog []config.CatalogDaemonConfig, credentials config.FederationCredentialStore, configure ...func(*daemon.ServerConfig)) projectAccessFixture {
	t.Helper()
	cfg := daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: credentials, FederationCatalog: catalog,
		FederationAdoptionPublish: func(ctx context.Context, c config.CatalogDaemonConfig, raw []byte, preview bool) (api.FederationPublicationBody, error) {
			h, err := federation.NewHubClient(ctx, c)
			if err != nil {
				return api.FederationPublicationBody{}, err
			}
			return h.PublishFederationProject(ctx, raw, preview)
		},
		FederationAdoptionSync: func(ctx context.Context, b db.FederationBinding, c config.FederationCredential, pulled func(int64, []db.Event), drained func(context.Context, db.FederationBinding) error) error {
			return federation.SyncFederationOnceWithCallbacks(ctx, store, b, c, client.Opts{}, pulled, drained)
		},
	}
	for _, apply := range configure {
		apply(&cfg)
	}
	server := daemon.NewServer(cfg)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	return projectAccessFixture{store: store, server: endpoint}
}

// Exact attached retries must keep the old operation epoch until authenticated
// reset sync drains and the native finalizer proves the activated credential.
func TestAdoptionAttachedSignedResetRetry(t *testing.T) {
	enrollments := 0
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == "POST" && r.URL.Path == "/api/v1/federation/enrollments" {
			enrollments++
		}
		next.ServeHTTP(w, r)
	})
	catalog := f.hub.catalog(f.localOrigin(t))
	credentials := config.DefaultFederationCredentialStore()
	drained := 0
	retain := func(context.Context, db.FederationBinding) error {
		drained++
		return errors.New("injected crash before fence release")
	}
	f.local = newAdoptionLocalDaemonUsing(t, f.store, catalog, credentials, func(cfg *daemon.ServerConfig) {
		cfg.FederationAdoptionSync = func(ctx context.Context, b db.FederationBinding, c config.FederationCredential, pulled func(int64, []db.Event), _ func(context.Context, db.FederationBinding) error) error {
			return federation.SyncFederationOnceWithCallbacks(ctx, f.store, b, c, client.Opts{}, pulled, retain)
		}
	})
	for range 4 {
		status, raw := adoptionConnect(t, f, false)
		require.Equal(t, 200, status, string(raw))
		require.Contains(t, string(raw), `"status":"pending"`)
		if drained > 0 {
			break
		}
	}
	require.Positive(t, drained)
	op, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.NoError(t, err)
	require.Equal(t, "attached", op.Phase)
	binding, err := f.store.FederationBindingByProject(t.Context(), f.project.ID)
	require.NoError(t, err)
	checkpoint, err := f.hub.store.(interface {
		CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
	}).CreateRelayReset(t.Context(), binding.RelayConfig.BindingUID, f.signer)
	require.NoError(t, err)
	_, err = f.hub.store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}).ExecContext(t.Context(), "DELETE FROM events WHERE project_id=?", op.HubProjectID)
	require.NoError(t, err)
	credential, found, err := credentials.FederationCredential(t.Context(), f.project.UID)
	require.NoError(t, err)
	require.True(t, found)
	for range 4 {
		_ = federation.SyncFederationOnceWithCallbacks(t.Context(), f.store, binding, credential, client.Opts{}, nil, retain)
		binding, err = f.store.FederationBindingByProject(t.Context(), f.project.ID)
		require.NoError(t, err)
		if binding.RelayConfig.ResetEpoch == checkpoint.Translation.Authority.Epoch {
			break
		}
	}
	require.Greater(t, binding.RelayConfig.ResetEpoch, op.ResetEpoch)
	retained, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.NoError(t, err)
	require.Equal(t, op, retained, "sync alone cannot advance the operation epoch")
	_, _, err = f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Author: "local-member", Title: "Still fenced after reset"})
	require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
	before := enrollments
	f.local = newAdoptionLocalDaemon(t, f.store, catalog)
	for range 4 {
		status, raw := adoptionConnect(t, f, false)
		require.Equal(t, 200, status, string(raw))
		if bytes.Contains(raw, []byte(`"status":"connected"`)) {
			break
		}
	}
	_, err = f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Equal(t, before, enrollments, "attached reset retry must reuse its activated hop")
}

func adoptionConnect(t *testing.T, f adoptionFixture, preview bool) (int, []byte) {
	t.Helper()
	body := bridgeConnectBody(f.project.Name, preview)
	body["adopt_existing"] = true
	status, _, raw := f.local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, bridgeOwnerHeaders)
	return status, raw
}

func TestAdoptionAdmissionBeforePause(t *testing.T) {
	for _, tc := range []struct {
		name, field, code string
		value             any
	}{
		{"old hub", "relay_publication_version", "federation_publication_unsupported", 0},
		{"signing required", "federation_signing_required", "hub_signing_required", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				if r.URL.Path != "/api/v1/instance" {
					next.ServeHTTP(w, r)
					return
				}
				response := httptest.NewRecorder()
				next.ServeHTTP(response, r)
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				body[tc.field] = tc.value
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			})
			for _, preview := range []bool{true, false} {
				status, raw := adoptionConnect(t, f, preview)
				require.Equal(t, 409, status, string(raw))
				require.Equal(t, tc.code, bridgeErrorCode(t, raw))
				_, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
				require.ErrorIs(t, err, db.ErrNotFound)
				_, err = os.Stat(filepath.Join(f.home, "federation", "adoptions"))
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestAdoptionDoesNotFollowCredentialRedirect(t *testing.T) {
	requests := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(target.Close)
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, _ http.Handler) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect) //nolint:gosec // The target is another test-owned loopback server.
	})
	status, raw := adoptionConnect(t, f, false)
	require.NotEqual(t, 200, status, string(raw))
	require.Zero(t, requests, "selected account credentials must never follow redirects")
	_, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestAdoptionFrozenResume(t *testing.T) {
	t.Run("preflight", func(t *testing.T) {
		f := newAdoptionFixture(t, nil)
		before, err := f.store.MaxEventID(t.Context())
		require.NoError(t, err)
		status, raw := adoptionConnect(t, f, true)
		require.Equal(t, 200, status, string(raw))
		var body api.FederationBridgeBody
		require.NoError(t, json.Unmarshal(raw, &body))
		require.Equal(t, "ready", body.Status)
		require.Contains(t, string(raw), `"visibility":"all"`)
		require.Contains(t, string(raw), `"issues":1`)
		require.Contains(t, string(raw), "pause")
		_, err = f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = os.Stat(filepath.Join(f.home, "federation", "adoptions"))
		require.ErrorIs(t, err, os.ErrNotExist)
		after, err := f.store.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
		_, err = f.hub.store.ProjectByUID(t.Context(), f.project.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
	})
	t.Run("lost response rename and uncertain refusal", func(t *testing.T) {
		var f adoptionFixture
		var envelopes [][]byte
		refuse := false
		f = newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if r.Method == "POST" && r.URL.Path == publicationPath {
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var request struct {
					Preflight bool           `json:"preflight"`
					Envelope  jsontext.Value `json:"envelope"`
				}
				require.NoError(t, json.Unmarshal(raw, &request))
				if !request.Preflight {
					op, err := f.store.(db.FederationAdoptionStore).FederationAdoption(r.Context(), f.project.ID)
					require.NoError(t, err)
					require.Equal(t, "transmitting", op.Phase)
					saved, err := config.ReadFederationAdoptionEnvelope(f.home, op.OperationUID, op.Digest)
					require.NoError(t, err)
					require.Equal(t, bytes.TrimSpace(saved), bytes.TrimSpace(request.Envelope))
					envelopes = append(envelopes, saved)
					if refuse {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(403)
						_, _ = w.Write([]byte(`{"error":{"code":"token_invalid","message":"refused"}}`))
						return
					}
					if len(envelopes) == 1 {
						recorder := httptest.NewRecorder()
						next.ServeHTTP(recorder, r)
						require.Equal(t, 200, recorder.Code, recorder.Body.String())
						w.WriteHeader(503)
						return
					}
				}
			}
			if r.URL.Path == "/api/v1/projects/resolve" || r.URL.Path == "/api/v1/projects/init" {
				t.Error("known publication must never resolve or create by name")
			}
			next.ServeHTTP(w, r)
		})
		status, raw := adoptionConnect(t, f, false)
		require.GreaterOrEqual(t, status, 500, string(raw))
		op, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
		require.NoError(t, err)
		require.Equal(t, "transmitting", op.Phase)
		status, _, raw = f.local.request(t, "GET", "/api/v1/federation/bridges/"+f.project.Name, "", nil, bridgeOwnerHeaders)
		require.Equal(t, 200, status, string(raw))
		require.Contains(t, string(raw), "adoption_pending")
		var pending api.FederationBridgeStatusBody
		require.NoError(t, json.Unmarshal(raw, &pending))
		require.Equal(t, "local-member", pending.LocalAccount)
		changedActor := bridgeConnectBody(f.project.Name, false)
		changedActor["adopt_existing"] = true
		changedActor["actor"] = "other-member"
		status, _, raw = f.local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", changedActor, bridgeOwnerHeaders)
		require.Equal(t, 409, status, string(raw), "an exact retry cannot change the selected local account")
		still, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
		require.NoError(t, err)
		require.Equal(t, op, still)
		status, _, raw = f.local.request(t, "POST", "/api/v1/federation/bridges/"+f.project.Name+"/disconnect", "", map[string]any{}, bridgeOwnerHeaders)
		require.Equal(t, 409, status, string(raw))
		refuse = true
		status, raw = adoptionConnect(t, f, false)
		require.GreaterOrEqual(t, status, 400, string(raw))
		still, err = f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
		require.NoError(t, err)
		require.Equal(t, op, still)
		refuse = false
		published, err := f.hub.store.ProjectByUID(t.Context(), f.project.UID)
		require.NoError(t, err)
		_, err = f.hub.store.RenameProject(t.Context(), published.ID, "renamed-project")
		require.NoError(t, err)
		_, _, err = f.hub.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: published.ID, Title: "Later hub edit", Author: "member"})
		require.NoError(t, err)
		status, raw = adoptionConnect(t, f, false)
		require.Equal(t, 200, status, string(raw))
		var result api.FederationBridgeBody
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Equal(t, published.ID, result.HubProjectID)
		require.Equal(t, f.project.UID, result.HubProjectUID)
		require.NotEmpty(t, envelopes)
		for _, envelope := range envelopes {
			require.Equal(t, envelopes[0], envelope)
		}
		p, err := f.hub.store.ProjectByUID(t.Context(), f.project.UID)
		require.NoError(t, err)
		require.Equal(t, "renamed-project", p.Name)
		_, err = f.hub.store.ProjectByName(t.Context(), f.project.Name)
		require.ErrorIs(t, err, db.ErrNotFound)
		require.NotContains(t, string(raw), f.home)
	})
	for _, scenario := range []struct {
		phase   string
		corrupt bool
	}{{"preparing", false}, {"frozen", false}, {"transmitting", false}, {"frozen", true}, {"transmitting", true}} {
		t.Run(fmt.Sprintf("artifact %s corrupt=%t", scenario.phase, scenario.corrupt), func(t *testing.T) {
			phase := scenario.phase
			f := newAdoptionFixture(t, nil)
			op := db.FederationAdoptionOperation{OperationUID: "01K00000000000000000000005", SourceInstanceUID: f.store.InstanceUID(), ProjectUID: f.project.UID, TargetInstanceUID: f.hub.store.InstanceUID(), TargetOrigin: f.localOrigin(t), CatalogName: "selected-hub", Account: "member", ProjectName: f.project.Name, ProjectID: f.project.ID, Phase: "preparing"}
			native := f.store.(db.FederationAdoptionStore)
			_, err := native.BeginFederationAdoption(t.Context(), op)
			require.NoError(t, err)
			if phase != "preparing" {
				document, err := federationpublication.Capture(t.Context(), f.store, f.project.ID, db.FederationPublication{OperationUID: op.OperationUID, SourceInstanceUID: op.SourceInstanceUID, ProjectUID: op.ProjectUID, TargetInstanceUID: op.TargetInstanceUID, ProjectName: op.ProjectName})
				require.NoError(t, err)
				raw, err := federationpublication.Encode(document)
				require.NoError(t, err)
				path, err := config.WriteFederationAdoptionEnvelope(f.home, op.OperationUID, raw)
				require.NoError(t, err)
				next := op
				next.Phase = "frozen"
				next.Digest = document.Digest
				next.EnvelopePath = path
				require.NoError(t, native.AdvanceFederationAdoption(t.Context(), op, next))
				op = next
				if phase == "transmitting" {
					next.Phase = phase
					require.NoError(t, native.AdvanceFederationAdoption(t.Context(), op, next))
					op = next
				}
				if scenario.corrupt {
					changed := bytes.ReplaceAll(raw, []byte("Retained source issue"), []byte("Tampered source issue"))
					require.NotEqual(t, raw, changed)
					_, err = config.WriteFederationAdoptionEnvelope(f.home, op.OperationUID, changed)
					require.NoError(t, err)
					_, err = config.ReadFederationAdoptionEnvelope(f.home, op.OperationUID, op.Digest)
					require.NoError(t, err, "config checks only the header; daemon must validate the complete codec")
				} else {
					require.NoError(t, os.Remove(path))
				}
			}
			status, raw := adoptionConnect(t, f, false)
			if phase == "transmitting" || scenario.corrupt {
				require.Equal(t, 409, status, string(raw))
				require.Contains(t, string(raw), "recovery")
				still, err := native.FederationAdoption(t.Context(), f.project.ID)
				require.NoError(t, err)
				require.Equal(t, op, still)
				_, err = f.hub.store.ProjectByUID(t.Context(), f.project.UID)
				require.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.Equal(t, 200, status, string(raw))
			}
		})
	}
}

func (f adoptionFixture) localOrigin(t *testing.T) string {
	t.Helper()
	// Status is intentionally unavailable before an operation; retain the
	// selected origin by observing the local server's read-only preflight.
	status, raw := adoptionConnect(t, f, true)
	require.Equal(t, 200, status, string(raw))
	var body api.FederationBridgeBody
	require.NoError(t, json.Unmarshal(raw, &body))
	return body.HubURL
}

func TestAdoptionDisconnectRecovery(t *testing.T) {
	for _, mode := range []string{"commit_then_404", "commit_then_plain_404", "definite_invalid"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				if r.Method == "POST" && r.URL.Path == publicationPath {
					if strings.HasPrefix(mode, "commit_then_") {
						recorder := httptest.NewRecorder()
						next.ServeHTTP(recorder, r)
						require.Equal(t, 200, recorder.Code, recorder.Body.String())
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(404)
						if mode == "commit_then_404" {
							_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"concealed after commit"}}`))
						} else {
							_, _ = w.Write([]byte("not found"))
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":{"code":"federation_publication_invalid","message":"invalid"}}`))
					return
				}
				next.ServeHTTP(w, r)
			})
			status, raw := adoptionConnect(t, f, false)
			require.GreaterOrEqual(t, status, 400, string(raw))
			native := f.store.(db.FederationAdoptionStore)
			op, err := native.FederationAdoption(t.Context(), f.project.ID)
			require.NoError(t, err)
			status, _, raw = f.local.request(t, "POST", "/api/v1/federation/bridges/"+f.project.Name+"/disconnect", "", map[string]any{}, bridgeOwnerHeaders)
			if strings.HasPrefix(mode, "commit_then_") {
				require.Equal(t, "transmitting", op.Phase)
				require.Equal(t, 409, status, string(raw))
				_, err = f.hub.store.ProjectByUID(t.Context(), f.project.UID)
				require.NoError(t, err)
				still, err := native.FederationAdoption(t.Context(), f.project.ID)
				require.NoError(t, err)
				require.Equal(t, op, still)
			} else {
				require.Equal(t, "rejected", op.Phase)
				require.Equal(t, 200, status, string(raw))
				_, err = native.FederationAdoption(t.Context(), f.project.ID)
				require.ErrorIs(t, err, db.ErrNotFound)
				_, _, err = f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Writable after definite rejection", Author: "local-member"})
				require.NoError(t, err)
			}
		})
	}
}

func TestAdoptionDisconnectReportsCleanupAfterCancellation(t *testing.T) {
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == "POST" && r.URL.Path == publicationPath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"federation_publication_invalid","message":"invalid"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
	status, raw := adoptionConnect(t, f, false)
	require.Equal(t, 400, status, string(raw))
	native := f.store.(db.FederationAdoptionStore)
	op, err := native.FederationAdoption(t.Context(), f.project.ID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(op.EnvelopePath))
	require.NoError(t, os.Symlink("missing-artifact", op.EnvelopePath))
	status, _, raw = f.local.request(t, "POST", "/api/v1/federation/bridges/"+f.project.Name+"/disconnect", "", map[string]any{}, bridgeOwnerHeaders)
	require.Equal(t, 500, status, string(raw))
	require.Equal(t, "federation_adoption_cleanup_failed", bridgeErrorCode(t, raw))
	require.Contains(t, string(raw), "pause was released")
	require.Contains(t, string(raw), op.OperationUID)
	require.NotContains(t, string(raw), "remain paused")
	require.NotContains(t, string(raw), f.home)
	_, err = native.FederationAdoption(t.Context(), f.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, _, err = f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Writable after artifact cleanup failure", Author: "local-member"})
	require.NoError(t, err)
}

func TestAdoptionAttachedRetryUsesActiveHop(t *testing.T) {
	var enrollments int
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.URL.Path == "/api/v1/federation/enrollments" && r.Method == "POST" {
			enrollments++
		}
		if strings.Contains(r.URL.Path, "/relay/") || strings.Contains(r.URL.Path, "/relay:") {
			w.WriteHeader(503)
			return
		}
		next.ServeHTTP(w, r)
	})
	status, raw := adoptionConnect(t, f, false)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"status":"pending"`)
	require.Equal(t, 1, enrollments)
	status, raw = adoptionConnect(t, f, false)
	require.Equal(t, 200, status, string(raw))
	require.Equal(t, 1, enrollments, "attached retry must sync the active hop rather than reenroll")
}

func TestAdoptionAttachedRevocationRecovery(t *testing.T) {
	blocked := true
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if blocked && (strings.Contains(r.URL.Path, "/relay/") || strings.Contains(r.URL.Path, "/relay:")) {
			w.WriteHeader(503)
			return
		}
		next.ServeHTTP(w, r)
	})
	status, raw := adoptionConnect(t, f, false)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"status":"pending"`)
	op, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.NoError(t, err)
	binding, err := f.store.FederationBindingByProject(t.Context(), f.project.ID)
	require.NoError(t, err)
	revoked := *binding.RelayConfig
	revoked.UpstreamRevoked = true
	_, err = f.store.SetRelayBindingConfig(db.WithFederationAdoptionAttachment(t.Context(), op), f.project.ID, revoked)
	require.NoError(t, err)
	_, _, err = f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Author: "local-member", Title: "Still paused"})
	require.ErrorIs(t, err, db.ErrFederationAdoptionPaused)
	blocked = false
	status, raw = adoptionConnect(t, f, false)
	require.Equal(t, 200, status, string(raw))
	binding, err = f.store.FederationBindingByProject(t.Context(), f.project.ID)
	require.NoError(t, err)
	require.False(t, binding.RelayConfig.UpstreamRevoked)
}

func TestAdoptionAttachedAccountTokenReplacement(t *testing.T) {
	blocked := true
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if blocked && (strings.Contains(r.URL.Path, "/relay/") || strings.Contains(r.URL.Path, "/relay:")) {
			w.WriteHeader(503)
			return
		}
		next.ServeHTTP(w, r)
	})
	origin := f.localOrigin(t)
	status, raw := adoptionConnect(t, f, false)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"status":"pending"`)
	before, err := f.store.FederationBindingByProject(t.Context(), f.project.ID)
	require.NoError(t, err)
	credentials := config.DefaultFederationCredentialStore()
	credential, found, err := credentials.FederationCredential(t.Context(), f.project.UID)
	require.NoError(t, err)
	require.True(t, found)
	parent, err := f.hub.store.ResolveAPIToken(t.Context(), "member-test-token")
	require.NoError(t, err)
	_, _, err = f.hub.store.RevokeAPIToken(t.Context(), parent.ID, "admin")
	require.NoError(t, err)
	replacement, _, err := f.hub.store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		Actor: parent.Actor, AdminActor: "admin", PlaintextToken: "replacement-account-test-token",
	})
	require.NoError(t, err)
	catalog := f.hub.catalog(origin)
	catalog[0].Token = "replacement-account-test-token"
	f.local = newAdoptionLocalDaemon(t, f.store, catalog)
	blocked = false
	var result api.FederationBridgeBody
	for range 6 {
		status, raw = adoptionConnect(t, f, false)
		require.Equal(t, 200, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &result))
		if result.Status == "connected" {
			break
		}
	}
	require.Equal(t, "connected", result.Status, "same-account token replacement must recover a paused cutover")
	after, err := f.store.FederationBindingByProject(t.Context(), f.project.ID)
	require.NoError(t, err)
	require.Equal(t, before.RelayConfig.BindingUID, after.RelayConfig.BindingUID)
	require.Equal(t, before.RelayConfig.ResetEpoch, after.RelayConfig.ResetEpoch)
	current, found, err := credentials.FederationCredential(t.Context(), f.project.UID)
	require.NoError(t, err)
	require.True(t, found && current.Equal(credential), "recovery must retain the exact activated transport credential")
	grants, err := f.hub.store.ListProjectFederationEnrollments(t.Context(), before.HubProjectID)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, &replacement.ID, grants[0].ParentTokenID)
}

func TestAdoptionRetryAfterCompletedCutover(t *testing.T) {
	publications := 0
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == "POST" && r.URL.Path == publicationPath {
			publications++
		}
		next.ServeHTTP(w, r)
	})
	var result api.FederationBridgeBody
	for range 4 {
		status, raw := adoptionConnect(t, f, false)
		require.Equal(t, 200, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &result))
		if result.Status == "connected" {
			break
		}
	}
	require.Equal(t, "connected", result.Status)
	before := publications
	_, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	artifacts, err := os.ReadDir(filepath.Join(f.home, "federation", "adoptions"))
	require.NoError(t, err)
	require.Empty(t, artifacts)
	_, err = f.hub.store.RenameProject(t.Context(), result.HubProjectID, "renamed-project")
	require.NoError(t, err)
	status, raw := adoptionConnect(t, f, false)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"status":"connected"`)
	require.Equal(t, before, publications, "a lost final response must not create a new publication operation")
	_, err = f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	artifacts, err = os.ReadDir(filepath.Join(f.home, "federation", "adoptions"))
	require.NoError(t, err)
	require.Empty(t, artifacts)
	catalog := f.hub.catalog(f.localOrigin(t))
	catalog[0].Token = "nonmember-test-token"
	other := f
	other.local = newAdoptionLocalDaemon(t, f.store, catalog)
	status, raw = adoptionConnect(t, other, false)
	require.Equal(t, 409, status, string(raw))
	require.Equal(t, before, publications)
	catalog = f.hub.catalog(f.hub.root.server.URL)
	other.local = newAdoptionLocalDaemon(t, f.store, catalog)
	status, raw = adoptionConnect(t, other, false)
	require.Equal(t, 409, status, string(raw), "same hub identity at a changed destination still refuses")
	require.Equal(t, before, publications)
	status, _, raw = f.local.request(t, "POST", "/api/v1/federation/bridges/"+f.project.Name+"/disconnect", "", map[string]any{}, bridgeOwnerHeaders)
	require.Equal(t, 200, status, string(raw))
	_, err = f.hub.store.ProjectByUID(t.Context(), f.project.UID)
	require.NoError(t, err, "disconnect retains published hub content")
}

type adoptionCaptureProbe struct {
	db.Storage
	db.FederationAdoptionStore
	before func(context.Context)
}

func (s *adoptionCaptureProbe) BeginExportSnapshot(ctx context.Context) (db.Storage, func() error, error) {
	s.before(ctx)
	return s.Storage.(interface {
		BeginExportSnapshot(context.Context) (db.Storage, func() error, error)
	}).BeginExportSnapshot(ctx)
}

func TestAdoptionPausePrecedesCaptureAndMapsConflict(t *testing.T) {
	blocked := false
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if blocked && r.Method == "POST" && r.URL.Path == publicationPath {
			w.WriteHeader(503)
			return
		}
		next.ServeHTTP(w, r)
	})
	catalog := f.hub.catalog(f.localOrigin(t))
	blocked = true
	captures := 0
	probe := &adoptionCaptureProbe{Storage: f.store, FederationAdoptionStore: f.store.(db.FederationAdoptionStore), before: func(ctx context.Context) {
		op, err := f.store.(db.FederationAdoptionStore).FederationAdoption(ctx, f.project.ID)
		require.NoError(t, err)
		require.Equal(t, "preparing", op.Phase)
		captures++
	}}
	f.local = newAdoptionLocalDaemon(t, probe, catalog)
	status, raw := adoptionConnect(t, f, false)
	require.GreaterOrEqual(t, status, 500, string(raw))
	require.Equal(t, 1, captures)
	status, _, raw = f.local.request(t, "POST", fmt.Sprintf("/api/v1/projects/%d/issues", f.project.ID), "", map[string]any{"title": "Cannot enter frozen source", "actor": "local-member"}, bridgeOwnerHeaders)
	require.Equal(t, 409, status, string(raw))
	require.Contains(t, string(raw), "federation_adoption_paused")
}

type adoptionActivationFailure struct {
	config.FederationManagedCredentialStore
	fail bool
}

func (s *adoptionActivationFailure) ReplaceFederationCredential(ctx context.Context, r config.FederationCredentialReplacement) error {
	if s.fail {
		s.fail = false
		return errors.New("injected credential activation failure")
	}
	return s.FederationManagedCredentialStore.(config.FederationCredentialReplacer).ReplaceFederationCredential(ctx, r)
}

func TestAdoptionCredentialActivationFailureRetainsCandidate(t *testing.T) {
	f := newAdoptionFixture(t, nil)
	catalog := f.hub.catalog(f.localOrigin(t))
	credentials := &adoptionActivationFailure{FederationManagedCredentialStore: config.DefaultFederationCredentialStore(), fail: true}
	f.local = newAdoptionLocalDaemonUsing(t, f.store, catalog, credentials)
	status, raw := adoptionConnect(t, f, false)
	require.GreaterOrEqual(t, status, 500, string(raw))
	op, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.NoError(t, err)
	require.Equal(t, "published", op.Phase)
	retained, found, err := credentials.FederationCredential(t.Context(), f.project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, retained.RelayEnrollmentPending)
	require.NotEmpty(t, retained.Token)
	_, err = config.ReadFederationAdoptionEnvelope(f.home, op.OperationUID, op.Digest)
	require.NoError(t, err)
	status, raw = adoptionConnect(t, f, false)
	require.Equal(t, 200, status, string(raw))
	active, found, err := credentials.FederationCredential(t.Context(), f.project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, retained.Token, active.Token)
	require.False(t, active.RelayEnrollmentPending)
	require.NotContains(t, string(raw), retained.Token)
}

func TestAdoptionPublishedCandidateBlocksLegacyLeavePreparation(t *testing.T) {
	enableFailed := false
	f := newAdoptionFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/federation/enable") && !enableFailed {
			enableFailed = true
			http.Error(w, "transient enable failure", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
	catalog := f.hub.catalog(f.localOrigin(t))
	credentials := config.DefaultFederationCredentialStore()
	f.local = newAdoptionLocalDaemonUsing(t, f.store, catalog, credentials)
	status, raw := adoptionConnect(t, f, false)
	require.GreaterOrEqual(t, status, 500, string(raw))
	op, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.NoError(t, err)
	require.Equal(t, "published", op.Phase)
	candidate, found, err := credentials.FederationCredential(t.Context(), f.project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, candidate.RelayEnrollmentPending)
	_, err = f.store.FederationBindingByProject(t.Context(), f.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "the failure is before a local relay binding exists")

	leavePath := fmt.Sprintf("/api/v1/federation/replicas/%d/actions/leave", f.project.ID)
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"preflight", map[string]any{"disposition": "detach", "preflight": true, "actor": "local-member"}},
		{"prepare", map[string]any{"disposition": "detach", "prepare": true, "actor": "local-member"}},
	} {
		status, _, raw = f.local.request(t, http.MethodPost, leavePath, "", tc.body, bridgeOwnerHeaders)
		if status != http.StatusConflict {
			t.Errorf("leave %s status = %d, want %d: %s", tc.name, status, http.StatusConflict, raw)
		} else if code := bridgeErrorCode(t, raw); code != "federation_adoption_paused" {
			t.Errorf("leave %s error code = %q, want federation_adoption_paused: %s", tc.name, code, raw)
		}
		current, currentFound, readErr := credentials.FederationCredential(t.Context(), f.project.UID)
		if readErr != nil || !currentFound || !candidate.Equal(current) {
			t.Errorf("leave %s changed the retained adoption credential: found=%t err=%v", tc.name, currentFound, readErr)
		}
	}

	status, raw = adoptionConnect(t, f, false)
	require.Equal(t, http.StatusOK, status, string(raw), "exact retry must remain able to attach the published project")
	active, found, err := credentials.FederationCredential(t.Context(), f.project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, active.LeavePending)
}

func TestPreparedLegacyLeaveBlocksFederationAdoption(t *testing.T) {
	f := newAdoptionFixture(t, nil)
	origin := f.localOrigin(t)
	catalog := f.hub.catalog(origin)
	credentials := config.DefaultFederationCredentialStore()
	f.local = newAdoptionLocalDaemonUsing(t, f.store, catalog, credentials)
	candidate := config.FederationCredential{
		HubURL: origin, HubProjectID: f.hub.root.private.ID, Token: "ordinary-test-token",
		Actor: "member", Capabilities: "claim,pull,push", ManagedByConfig: true,
		HubCatalog: catalog[0].Name, HubProjectName: f.hub.root.private.Name,
		SpokeProjectName: f.project.Name, RequestedActor: "local-member",
		AllowInsecure: true, RelayEnrollmentPending: true,
	}
	require.NoError(t, daemon.ReserveFederationReplicaCredential(t.Context(), f.store, credentials,
		daemon.ReserveFederationReplicaCredentialParams{HubProjectUID: f.project.UID, ProjectName: f.project.Name, Credential: candidate}))
	prepared, err := daemon.PrepareFederationReplicaLeave(t.Context(), f.store, credentials, f.project.ID)
	require.NoError(t, err)
	require.True(t, prepared.ManagedReservationFound)
	require.True(t, prepared.ManagedReservation.Credential.LeavePending)

	status, raw := adoptionConnect(t, f, false)
	require.Equal(t, http.StatusConflict, status, string(raw))
	require.Equal(t, "federation_adoption_conflict", bridgeErrorCode(t, raw))
	_, err = f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "a rejected admission must not install an adoption fence")
	_, err = f.hub.store.ProjectByUID(t.Context(), f.project.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "a prepared leave must prevent hub publication")
}

type adoptionCredentialReadProbe struct {
	config.FederationCredentialStore
	reads, changeAt int
}

func (s *adoptionCredentialReadProbe) FederationCredential(ctx context.Context, projectUID string) (config.FederationCredential, bool, error) {
	credential, found, err := s.FederationCredentialStore.FederationCredential(ctx, projectUID)
	s.reads++
	if s.reads == s.changeAt {
		credential.LeavePending = true
	}
	return credential, found, err
}

func TestAdoptionFinalizationRevalidatesCredentialAtCommit(t *testing.T) {
	f := newAdoptionFixture(t, nil)
	catalog := f.hub.catalog(f.localOrigin(t))
	credentials := config.DefaultFederationCredentialStore()
	probe := &adoptionCredentialReadProbe{FederationCredentialStore: credentials, changeAt: 3}
	drained := 0
	f.local = newAdoptionLocalDaemonUsing(t, f.store, catalog, credentials, func(cfg *daemon.ServerConfig) {
		finalizerConfig := *cfg
		finalizerConfig.FederationCredentials = probe
		cfg.FederationAdoptionSync = func(ctx context.Context, b db.FederationBinding, c config.FederationCredential, pulled func(int64, []db.Event), _ func(context.Context, db.FederationBinding) error) error {
			return federation.SyncFederationOnceWithCallbacks(ctx, f.store, b, c, client.Opts{}, pulled, func(ctx context.Context, current db.FederationBinding) error {
				drained++
				probe.reads = 0
				return daemon.FinalizeFederationAdoptionAfterSync(ctx, finalizerConfig, current)
			})
		}
	})
	for range 4 {
		status, raw := adoptionConnect(t, f, false)
		require.Equal(t, 200, status, string(raw))
		if drained > 0 {
			require.Contains(t, string(raw), `"status":"pending"`)
			break
		}
	}
	require.Positive(t, drained)
	require.GreaterOrEqual(t, probe.reads, 3, "native commit must re-read the active credential")
	op, err := f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.NoError(t, err)
	_, err = config.ReadFederationAdoptionEnvelope(f.home, op.OperationUID, op.Digest)
	require.NoError(t, err, "failed live credential proof retains the frozen recovery artifact")
	probe.changeAt = 0
	status, raw := adoptionConnect(t, f, false)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"status":"connected"`)
	_, err = f.store.(db.FederationAdoptionStore).FederationAdoption(t.Context(), f.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
