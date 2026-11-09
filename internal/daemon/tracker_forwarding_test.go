package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/tracker"
)

type trackerForwardRequest struct {
	method, path string
	header       http.Header
	body         []byte
}

type trackerForwardFixture struct {
	store       *sqlitestore.Store
	project     db.Project
	hubProject  db.Project
	cfg         ServerConfig
	credentials *bridgeConnectCredentialStore
	meta        api.ProjectFederationBody
	result      api.TrackerRelayResult
	// Overrides run only in the stub hub, at the real network boundary.
	onMetadata  func(context.Context, *api.ProjectFederationBody) error
	respond     func(http.ResponseWriter, *http.Request) bool
	mu          sync.Mutex
	requests    []trackerForwardRequest
	setupErrors []error
}

func newTrackerForwardFixture(t *testing.T, kind string) (*trackerForwardFixture, api.TrackerRelayBody) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "fixture-environment-owner-secret")
	t.Setenv("KATA_AUTH_TOKEN_FILE", "")
	t.Setenv("TRACKER_FORWARD_SIGNING_KEY", strings.Repeat("s", 64))
	open := func(name string) *sqlitestore.Store {
		store, err := sqlitestore.Open(t.Context(), filepath.Join(home, name))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		return store
	}
	hub, spoke := open("hub.db"), open("spoke.db")
	hp, err := hub.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = spoke.CreateProject(t.Context(), "padding-project")
	require.NoError(t, err)
	sp, err := spoke.CreateProjectWithUID(t.Context(), "spoke-project", hp.UID)
	require.NoError(t, err)
	require.NotEqual(t, hp.ID, sp.ID, "portable identity must not rely on matching local IDs")
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: hp.UID, AuthorityUID: hub.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, spoke.PinRootAuthority(t.Context(), pin))
	route := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", UpstreamInstanceUID: hub.InstanceUID(), AuthorityUID: hub.InstanceUID(), HubPath: []string{hub.InstanceUID(), spoke.InstanceUID()}, LocalActor: "member", ServeDownstream: true, ResetEpoch: 1}
	f := &trackerForwardFixture{store: spoke, project: sp, hubProject: hp, credentials: &bridgeConnectCredentialStore{credentials: map[string]config.FederationCredential{}}}
	f.meta = api.ProjectFederationBody{TrackerControlVersion: tracker.ControlProtocolVersion, ProjectID: hp.ID, ProjectUID: hp.UID, ProjectName: "hub-project", Relay: &api.RelayHandshake{ProtocolVersion: route.ProtocolVersion, BindingUID: route.BindingUID, UpstreamInstanceUID: route.UpstreamInstanceUID, ResetEpoch: route.ResetEpoch, HubPath: append([]string(nil), route.HubPath...), Root: pin}}
	operationUID := "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	f.result = api.TrackerRelayResult{RootInstanceUID: pin.AuthorityUID, ProjectUID: hp.UID}
	if kind == "discover" {
		f.result.Connections = &api.TrackerConnectionsBody{RootInstanceUID: pin.AuthorityUID, ProjectUID: hp.UID, Connections: []tracker.PublicDescriptor{}}
	} else {
		f.result.Operation = &api.TrackerOperationOut{RootInstanceUID: pin.AuthorityUID, ProjectUID: hp.UID, UID: operationUID, Selection: tracker.Selection{OwnerInstanceUID: pin.AuthorityUID, ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Generation: "g1"}, Kind: tracker.ControlDisable, State: tracker.ControlComplete, Result: &tracker.ControlResult{Code: "ok"}}
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "fixture read failed", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, trackerForwardRequest{r.Method, r.URL.Path, r.Header.Clone(), raw})
		f.mu.Unlock()
		if f.respond != nil && f.respond(w, r) {
			return
		}
		var value any = f.result
		if strings.HasSuffix(r.URL.Path, "/metadata") {
			meta := f.meta
			if f.onMetadata != nil {
				if err := f.onMetadata(r.Context(), &meta); err != nil {
					f.mu.Lock()
					f.setupErrors = append(f.setupErrors, err)
					f.mu.Unlock()
					http.Error(w, "fixture metadata setup failed", http.StatusInternalServerError)
					return
				}
			}
			value = meta
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, value); err != nil {
			t.Errorf("encode fixture response: %v", err)
		}
	}))
	t.Cleanup(endpoint.Close)
	_, err = spoke.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: sp.ID, Role: db.FederationRoleSpoke, HubURL: endpoint.URL, HubProjectID: hp.ID, HubProjectUID: hp.UID, Actor: "member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	_, err = spoke.SetRelayBindingConfig(t.Context(), sp.ID, route)
	require.NoError(t, err)
	f.credentials.credentials[sp.UID] = config.FederationCredential{HubURL: endpoint.URL, HubProjectID: hp.ID, Token: "fixture-enrollment-secret", Actor: "member", Capabilities: "pull,push", Signing: &signingpolicy.Source{KeyID: "fixture-key", KeyEnv: "TRACKER_FORWARD_SIGNING_KEY", HubURL: endpoint.URL}}
	f.cfg = ServerConfig{DB: spoke, Auth: config.AuthConfig{Token: "fixture-local-daemon-secret"}, FederationCredentials: f.credentials, FederationHubClient: trackerForwardCredentialClient}
	payload := api.TrackerRelayPayload{Kind: kind, UID: operationUID}
	if kind == "execute" {
		payload.Operation = &api.TrackerOperationBody{UID: operationUID, Kind: tracker.ControlDisable, Provider: "notion", Resource: "notion:source", Selection: &tracker.Selection{OwnerInstanceUID: pin.AuthorityUID, ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Generation: "g1"}}
	}
	digest, err := trackerRelayPayloadDigest(payload, kind == "execute")
	require.NoError(t, err)
	assertion := tracker.RelayAssertion{Version: tracker.ControlProtocolVersion, RootInstanceUID: pin.AuthorityUID, ProjectUID: hp.UID, Source: tracker.Principal{AuthorityInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAT", Kind: tracker.SubjectAccount, Subject: "member"}, IssuerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAT", AudienceInstanceUID: spoke.InstanceUID(), OperationUID: operationUID, RequestDigest: digest, ExpiresAt: time.Now().Add(30 * time.Second).Unix(), Path: []tracker.RelayHop{{InstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAT", BindingUID: "01ARZ3NDEKTSV4RRFFQ69G5FAU", ResetEpoch: 1}}}
	return f, api.TrackerRelayBody{Assertion: assertion, Payload: payload}
}

// federation imports daemon, so an internal daemon test cannot import it back.
// Keep CredentialHTTPClient's actual hook signature and use its production
// bearer-pinning/signing components, with fixture-only credential references.
func trackerForwardCredentialClient(ctx context.Context, credential config.FederationCredential) (*http.Client, error) {
	client, err := clientpkg.NewHTTPClientWithBearer(ctx, credential.HubURL, credential.Token, clientpkg.Opts{AllowInsecure: credential.AllowInsecure})
	if err != nil {
		return nil, err
	}
	if credential.Signing != nil {
		err = federationsigning.ConfigureClient(client, credential.HubURL, credential.Token, *credential.Signing)
	}
	return client, err
}

func (f *trackerForwardFixture) observed(t *testing.T) []trackerForwardRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Empty(t, f.setupErrors, "fixture mutation must succeed; its failure is not a forwarding refusal")
	return append([]trackerForwardRequest(nil), f.requests...)
}

// Catch wrong endpoint/ID translation, loss of qualified subject/payload, and
// accidental reuse of daemon credentials instead of the scoped enrollment.
func TestTrackerForwardRelayRoutesQualifiedCalls(t *testing.T) {
	for _, kind := range []string{"discover", "execute", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			f, body := newTrackerForwardFixture(t, kind)
			originalPath := append([]tracker.RelayHop(nil), body.Assertion.Path...)
			got, err := forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
			require.NoError(t, err)
			require.Equal(t, f.result, got)
			requests := f.observed(t)
			require.Len(t, requests, 2)
			base := fmt.Sprintf("/api/v1/projects/%d/federation/", f.hubProject.ID)
			require.Equal(t, http.MethodGet, requests[0].method)
			require.Equal(t, base+"metadata", requests[0].path)
			suffix := "tracker/read"
			if kind == "execute" {
				suffix = "tracker/control"
			}
			require.Equal(t, http.MethodPost, requests[1].method)
			require.Equal(t, base+suffix, requests[1].path)
			for _, request := range requests {
				require.Equal(t, "Bearer fixture-enrollment-secret", request.header.Get("Authorization"))
				require.NotEmpty(t, request.header.Get("Signature"))
				require.NotEmpty(t, request.header.Get("Content-Digest"))
				require.Equal(t, db.SupportedEventFeatures, request.header.Get(db.EventFeaturesHeader))
				require.NotContains(t, string(request.body), "fixture-local-daemon-secret")
				require.NotContains(t, string(request.body), "fixture-environment-owner-secret")
			}
			var submitted api.TrackerRelayBody
			require.NoError(t, json.Unmarshal(requests[1].body, &submitted))
			require.Equal(t, body.Payload, submitted.Payload)
			require.Equal(t, body.Assertion.Source, submitted.Assertion.Source)
			require.Equal(t, body.Assertion.RequestDigest, submitted.Assertion.RequestDigest)
			require.Len(t, submitted.Assertion.Path, len(originalPath)+1)
			require.Equal(t, originalPath, submitted.Assertion.Path[:len(originalPath)])
			require.Equal(t, originalPath, body.Assertion.Path, "forwarding must not mutate the caller's path")
			require.Equal(t, f.store.InstanceUID(), submitted.Assertion.IssuerInstanceUID)
			require.Equal(t, f.meta.Relay.UpstreamInstanceUID, submitted.Assertion.AudienceInstanceUID)
			require.NoError(t, submitted.Assertion.Validate(f.meta.Relay.UpstreamInstanceUID, f.store.InstanceUID(), f.meta.Relay.BindingUID, 1, true, time.Now()))
		})
	}
}

// Removing any negotiated identity check must allow a POST and fail this test.
func TestTrackerForwardRelayRejectsMetadataMismatch(t *testing.T) {
	cases := map[string]func(*api.ProjectFederationBody){
		"control-version": func(m *api.ProjectFederationBody) { m.TrackerControlVersion = 0 },
		"project-id":      func(m *api.ProjectFederationBody) { m.ProjectID++ },
		"project-uid":     func(m *api.ProjectFederationBody) { m.ProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAS" },
		"route":           func(m *api.ProjectFederationBody) { m.Relay.BindingUID = "01ARZ3NDEKTSV4RRFFQ69G5FAS" },
		"reset":           func(m *api.ProjectFederationBody) { m.Relay.ResetRequired = true },
		"root-key":        func(m *api.ProjectFederationBody) { m.Relay.Root.PublicKey = make([]byte, ed25519.PublicKeySize) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, body := newTrackerForwardFixture(t, "execute")
			f.onMetadata = func(_ context.Context, m *api.ProjectFederationBody) error { change(m); return nil }
			_, err := forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
			require.ErrorIs(t, err, errTrackerRelayUnavailable)
			require.Len(t, f.observed(t), 1, "failed negotiation must never submit a control")
		})
	}
}

func TestTrackerForwardRelayRejectsCredentialTargetMismatch(t *testing.T) {
	for _, mode := range []string{"origin", "project", "missing", "unsigned"} {
		t.Run(mode, func(t *testing.T) {
			f, body := newTrackerForwardFixture(t, "discover")
			credential := f.credentials.credentials[f.project.UID]
			switch mode {
			case "origin":
				credential.HubURL = "https://other.example"
			case "project":
				credential.HubProjectID++
			case "missing":
				credential.Token = ""
			case "unsigned":
				credential.Signing = nil
			}
			f.credentials.credentials[f.project.UID] = credential
			_, err := forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
			require.ErrorIs(t, err, errTrackerRelayUnavailable)
			require.Empty(t, f.observed(t), "mismatched transport must fail before any network request")
		})
	}
}

func TestTrackerForwardRelayRechecksAuthorityAfterMetadata(t *testing.T) {
	for _, mode := range []string{"binding-disabled", "upstream-revoked", "project-access"} {
		t.Run(mode, func(t *testing.T) {
			f, body := newTrackerForwardFixture(t, "execute")
			ctx := db.WithAdditionalTransactionFence(t.Context(), f.store.ProjectAccessTransactionFence("member", []string{f.project.UID}))
			f.onMetadata = func(ctx context.Context, _ *api.ProjectFederationBody) error {
				if mode == "project-access" {
					_, _, err := f.store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: f.project.UID, Visibility: "teams"}, "admin")
					return err
				}
				binding, err := f.store.FederationBindingByProject(ctx, f.project.ID)
				if err != nil {
					return err
				}
				if mode == "upstream-revoked" {
					route := *binding.RelayConfig
					route.UpstreamRevoked = true
					_, err = f.store.SetRelayBindingConfig(ctx, f.project.ID, route)
				} else {
					binding.Enabled = false
					_, err = f.store.UpsertFederationBinding(ctx, binding)
				}
				return err
			}
			_, err := forwardTrackerRelay(ctx, f.cfg, f.project, body)
			want := errTrackerRelayUnavailable
			if mode == "project-access" {
				want = tracker.ErrDenied
			}
			require.ErrorIs(t, err, want)
			require.Len(t, f.observed(t), 1, "revoked authority must stop submission after preflight")
		})
	}
}

func TestTrackerForwardRelayRequiresLiveSpokeAndPush(t *testing.T) {
	for _, mode := range []string{"disabled", "push-disabled"} {
		t.Run(mode, func(t *testing.T) {
			f, body := newTrackerForwardFixture(t, "execute")
			binding, err := f.store.FederationBindingByProject(t.Context(), f.project.ID)
			require.NoError(t, err)
			if mode == "disabled" {
				binding.Enabled = false
			} else {
				binding.PushEnabled = false
			}
			_, err = f.store.UpsertFederationBinding(t.Context(), binding)
			require.NoError(t, err)
			_, err = forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
			if mode == "disabled" {
				require.ErrorIs(t, err, errTrackerRelayUnavailable)
			} else {
				require.ErrorIs(t, err, tracker.ErrDenied)
			}
			require.Empty(t, f.observed(t))
		})
	}
}

func TestTrackerForwardRelayRejectsRedirects(t *testing.T) {
	for _, stage := range []string{"metadata", "control"} {
		for _, target := range []string{"same-origin", "other-origin"} {
			t.Run(stage+"/"+target, func(t *testing.T) {
				f, body := newTrackerForwardFixture(t, "execute")
				var trapCalls int
				var trapMu sync.Mutex
				trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { trapMu.Lock(); trapCalls++; trapMu.Unlock() }))
				t.Cleanup(trap.Close)
				f.respond = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path == "/trap" {
						trapMu.Lock()
						trapCalls++
						trapMu.Unlock()
						return true
					}
					if !strings.HasSuffix(r.URL.Path, "/"+stage) {
						return false
					}
					location := "/trap"
					if target == "other-origin" {
						location = trap.URL + "/trap"
					}
					http.Redirect(w, r, location, http.StatusTemporaryRedirect)
					return true
				}
				_, err := forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
				require.ErrorIs(t, err, errTrackerRelayUnavailable)
				trapMu.Lock()
				defer trapMu.Unlock()
				require.Zero(t, trapCalls, "redirect destinations must receive no credentialed request")
			})
		}
	}
}

func TestTrackerForwardRelayRejectsWrongResultIdentityOrShape(t *testing.T) {
	for _, mode := range []string{"root", "project", "operation-uid", "missing-operation", "mixed-shape", "nested-discovery"} {
		t.Run(mode, func(t *testing.T) {
			kind := "receipt"
			if mode == "nested-discovery" {
				kind = "discover"
			}
			f, body := newTrackerForwardFixture(t, kind)
			switch mode {
			case "root":
				f.result.RootInstanceUID = "01ARZ3NDEKTSV4RRFFQ69G5FAS"
			case "project":
				f.result.ProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAS"
			case "operation-uid":
				f.result.Operation.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAS"
			case "missing-operation":
				f.result.Operation = nil
			case "mixed-shape":
				f.result.Connections = &api.TrackerConnectionsBody{}
			case "nested-discovery":
				f.result.Connections.ProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAS"
			}
			_, err := forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
			require.ErrorIs(t, err, errTrackerRelayUnavailable)
			require.Len(t, f.observed(t), 2, "exercise response validation after actual submission")
		})
	}
}

// A full path or repeated local hop must be refused before negotiating upstream.
func TestTrackerForwardRelayRejectsUnappendablePath(t *testing.T) {
	for _, mode := range []string{"full", "cycle"} {
		t.Run(mode, func(t *testing.T) {
			f, body := newTrackerForwardFixture(t, "discover")
			if mode == "cycle" {
				body.Assertion.Path[0].InstanceUID = f.store.InstanceUID()
			} else {
				for len(body.Assertion.Path) < tracker.MaxControlHops {
					body.Assertion.Path = append(body.Assertion.Path, tracker.RelayHop{InstanceUID: fmt.Sprintf("%026d", len(body.Assertion.Path)), BindingUID: "01ARZ3NDEKTSV4RRFFQ69G5FAU", ResetEpoch: 1})
				}
			}
			_, err := forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
			require.ErrorIs(t, err, errTrackerRelayUnavailable)
			require.Empty(t, f.observed(t))
		})
	}
}

func TestTrackerForwardRelayPreservesClassifiedUpstreamErrors(t *testing.T) {
	for _, stage := range []string{"metadata", "control"} {
		t.Run(stage, func(t *testing.T) {
			f, body := newTrackerForwardFixture(t, "execute")
			f.respond = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/"+stage) {
					return false
				}
				api.WriteEnvelope(w, http.StatusForbidden, "tracker_denied", "fixture-upstream-secret-detail")
				return true
			}
			_, err := forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
			require.ErrorIs(t, err, tracker.ErrDenied)
			require.NotContains(t, err.Error(), "fixture-upstream-secret-detail")
			wantRequests := 1
			if stage == "control" {
				wantRequests = 2
			}
			require.Len(t, f.observed(t), wantRequests)
		})
	}
}

// Real variant axes: classified code and independent non-200 HTTP status.
// The oracle names contract error identities; upstream diagnostics never escape.
func TestTrackerHubRequestClassifiedEnvelopeProperty(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{
		{"tracker_operation_unsupported", errTrackerOperationUnsupported},
		{"tracker_binding_disabled", db.ErrIssueSyncNotEnabled},
		{"tracker_running", db.ErrIssueSyncAlreadyRunning},
		{"tracker_project_already_bound", db.ErrIssueSyncProjectAlreadyBound},
		{"tracker_denied", tracker.ErrDenied},
		{"tracker_operation_conflict", tracker.ErrControlConflict},
		{"tracker_operation_pending", tracker.ErrControlPending},
		{"tracker_capacity", tracker.ErrControlCapacity},
		{"tracker_selection_required", tracker.ErrSelectionRequired},
		{"tracker_outcome_unknown", db.ErrTrackerOutcomeUnknown},
		{"tracker_timeout", context.DeadlineExceeded},
		{"tracker_activation_required", db.ErrTrackerActivationRequired},
		{"tracker_binding_changed", db.ErrIssueSyncBindingChanged},
		{"not_found", db.ErrNotFound},
		{"validation", tracker.ErrInvalid},
		{"unknown-class", errTrackerRelayUnavailable},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, _ := strconv.Atoi(r.URL.Query().Get("status"))
		api.WriteEnvelope(w, status, r.URL.Query().Get("code"), "fixture-upstream-secret-detail")
	}))
	t.Cleanup(server.Close)
	hegel.Test(t, func(ht *hegel.T) {
		c := hegel.Draw(ht, hegel.SampledFrom(cases))
		status := hegel.Draw(ht, hegel.Integers(201, 599))
		query := url.Values{"status": {strconv.Itoa(status)}, "code": {c.code}}
		var result api.TrackerRelayResult
		err := trackerHubRequest(t.Context(), server.Client(), http.MethodPost, server.URL+"/?"+query.Encode(), struct{}{}, &result)
		want := c.want
		if status == http.StatusNoContent || status == http.StatusNotModified {
			want = errTrackerRelayUnavailable // HTTP forbids an envelope body.
		}
		require.ErrorIs(ht, err, want)
		require.NotContains(ht, err.Error(), "fixture-upstream-secret-detail")
	}, hegel.WithTestCases(150))
}

// Transport deadlines before an envelope arrives remain classified without
// forwarding a transport's diagnostic or credential-bearing target URL.
type trackerDeadlineTransport struct{}

func (trackerDeadlineTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("fixture private target: %w", context.DeadlineExceeded)
}
func TestTrackerHubRequestClassifiesTransportDeadline(t *testing.T) {
	client := &http.Client{Transport: trackerDeadlineTransport{}}
	var out api.TrackerRelayResult
	err := trackerHubRequest(t.Context(), client, http.MethodPost, "https://daemon.example/", struct{}{}, &out)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotContains(t, err.Error(), "fixture private target")
}

type trackerForwardDeadlineStore struct {
	db.Storage
	phase    string
	bindings int
}

func (s *trackerForwardDeadlineStore) FederationBindingByProject(ctx context.Context, id int64) (db.FederationBinding, error) {
	s.bindings++
	if (s.phase == "binding" && s.bindings == 1) || (s.phase == "recheck" && s.bindings == 2) {
		return db.FederationBinding{}, context.DeadlineExceeded
	}
	return s.Storage.FederationBindingByProject(ctx, id)
}
func (s *trackerForwardDeadlineStore) RootAuthority(ctx context.Context, project string) (db.RootKeyPin, error) {
	if s.phase == "root-pin" {
		return db.RootKeyPin{}, context.DeadlineExceeded
	}
	return s.Storage.RootAuthority(ctx, project)
}
func TestTrackerForwardPreparationPreservesDeadlineClassification(t *testing.T) {
	for _, phase := range []string{"binding", "recheck", "root-pin", "client"} {
		t.Run(phase, func(t *testing.T) {
			f, body := newTrackerForwardFixture(t, "execute")
			f.cfg.DB = &trackerForwardDeadlineStore{Storage: f.store, phase: phase}
			if phase == "client" {
				f.cfg.FederationHubClient = func(context.Context, config.FederationCredential) (*http.Client, error) {
					return nil, context.DeadlineExceeded
				}
			}
			_, err := forwardTrackerRelay(t.Context(), f.cfg, f.project, body)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			for _, request := range f.observed(t) {
				require.NotEqual(t, http.MethodPost, request.method, "preparation deadline cannot submit a control")
			}
		})
	}
}
