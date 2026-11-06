package federation_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

func serveBrowseFixture(t *testing.T, node *relayMatrixNode, store db.Storage, endpoint, folder string) {
	t.Helper()
	cfg := daemon.ServerConfig{DB: store, FederationCredentials: node.credentials, RootSigning: daemon.NewRootSigning(node.signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, DocbankFolder: folder}
	if endpoint != "" {
		cfg.Docbank = docbank.NewClient(endpoint, func() (string, error) { return "test-key", nil })
	}
	server := daemon.NewServer(cfg)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler := server.Handler()
	node.handler.Store(&handler)
}

type browseCapabilityStore struct {
	db.Storage
	pull bool
}

func (s browseCapabilityStore) AuthorizeFederationToken(ctx context.Context, token string, projectID int64, capability string) (db.FederationEnrollment, error) {
	if capability == "pull" && !s.pull || capability == "push" && s.pull {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	return s.Storage.AuthorizeFederationToken(ctx, token, projectID, capability)
}

func TestRelayDocbankBrowseRequiresPullNotPush(t *testing.T) {
	for _, pull := range []bool{false, true} {
		t.Run(fmt.Sprintf("pull=%t", pull), func(t *testing.T) {
			root, _, leaf, _ := newRelayClaimChain(t)
			vault := docbanktest.New(t)
			vault.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
			serveBrowseFixture(t, root, browseCapabilityStore{Storage: root.store, pull: pull}, vault.Server.URL, "/kata")
			for _, operation := range []string{"children", "resolve?path=/kata"} {
				status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/%s", leaf.project.ID, operation), "", "")
				if pull {
					require.Equal(t, http.StatusOK, status, string(raw))
				} else {
					require.Equal(t, http.StatusForbidden, status, string(raw))
					require.Empty(t, vault.Requests())
				}
			}
		})
	}
}

func TestRelayDocbankBrowseDefaultIsOriginFolderWithoutLocalConfiguration(t *testing.T) {
	root, relay, leaf, _ := newRelayClaimChain(t)
	vault := docbanktest.New(t)
	vault.Seed("/shared", docbank.Node{ID: 10, Name: "shared", Kind: "dir"}, nil)
	serveBrowseFixture(t, root, root.store, vault.Server.URL, "/shared")
	serveBrowseFixture(t, relay, relay.store, "", "/middle")
	serveBrowseFixture(t, leaf, leaf.store, "", "/personal")
	status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/children", leaf.project.ID), "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var page struct {
		Directory struct {
			Path string `json:"path"`
		} `json:"directory"`
		Items []any `json:"items"`
		Total int   `json:"total"`
		Limit int   `json:"limit"`
	}
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Equal(t, "/shared", page.Directory.Path)
	require.NotNil(t, page.Items)
	require.Empty(t, page.Items)
	require.Zero(t, page.Total)
	require.Equal(t, 100, page.Limit)
	for _, request := range vault.Requests() {
		require.True(t, strings.HasPrefix(request, "GET "), request)
	}
}

func TestRelayDocbankBrowseRequiresEveryHopCapability(t *testing.T) {
	for _, hop := range []string{"root", "relay"} {
		t.Run(hop, func(t *testing.T) {
			root, relay, leaf, _ := newRelayClaimChain(t)
			vault := docbanktest.New(t)
			serveRelayDocbank(t, root, vault)
			node := root
			if hop == "relay" {
				node = relay
			}
			original := *node.handler.Load()
			var reads atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/federation/docbank/capabilities") {
					// Model the pre-browse version1 wire shape. Other tests exercise
					// real enrollment authentication; this fixture tests negotiation.
					w.Header().Set("Content-Type", "application/json")
					_ = json.MarshalWrite(w, map[string]any{"version": 1, "project_uid": root.project.UID, "origin": root.store.InstanceUID()})
					return
				}
				if strings.Contains(r.URL.Path, "/federation/docbank/") && (strings.HasSuffix(r.URL.Path, "/resolve") || strings.HasSuffix(r.URL.Path, "/children")) {
					reads.Add(1)
				}
				original.ServeHTTP(w, r)
			})
			var wrapped http.Handler = handler
			node.handler.Store(&wrapped)
			for _, operation := range []string{"children", "resolve?path=/kata/file.pdf"} {
				status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/%s", leaf.project.ID, operation), "", "")
				require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
				require.Contains(t, string(raw), "docbank_forward_unsupported")
			}
			require.Zero(t, reads.Load())
			require.Empty(t, vault.Requests())
		})
	}
}

func TestRelayDocbankBrowseRevocationWhileReadingWithholdsResult(t *testing.T) {
	for _, hop := range []string{"root", "relay"} {
		for _, operation := range []string{"children", "resolve?path=/kata"} {
			t.Run(hop+"/"+operation, func(t *testing.T) {
				root, relay, leaf, _ := newRelayClaimChain(t)
				vault := docbanktest.New(t)
				vault.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
				serveRelayDocbank(t, root, vault)
				hub, child := root, relay
				if hop == "relay" {
					hub, child = relay, leaf
				}
				entry, err := hub.store.AuthorizeFederationToken(t.Context(), child.credential.Token, hub.project.ID, "pull")
				require.NoError(t, err)
				before, err := hub.store.ProjectAccessRevision(t.Context())
				require.NoError(t, err)
				revoked := make(chan error, 1)
				var once sync.Once
				vault.BeforeResponse = func(_, _ string) {
					once.Do(func() { revoked <- hub.store.RevokeFederationEnrollment(t.Context(), entry.ID) })
				}
				status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/%s", leaf.project.ID, operation), "", "")
				select {
				case err := <-revoked:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("synthetic revocation did not run")
				}
				require.GreaterOrEqual(t, status, 400, string(raw))
				require.NotContains(t, string(raw), `"directory"`)
				require.NotContains(t, string(raw), `"node"`)
				after, err := hub.store.ProjectAccessRevision(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after)
			})
		}
	}
}

func TestRelayDocbankBrowseWrongProjectAndOriginNeverReadVault(t *testing.T) {
	root, relay, _, _ := newRelayClaimChain(t)
	vault := docbanktest.New(t)
	serveRelayDocbank(t, root, vault)
	for _, query := range []url.Values{
		{"project_uid": {relay.store.InstanceUID()}, "origin": {root.store.InstanceUID()}, "path": {"/kata"}},
		{"project_uid": {root.project.UID}, "origin": {relay.store.InstanceUID()}, "path": {"/kata"}},
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/docbank/resolve?%s", root.http.URL, root.project.ID, query.Encode()), nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+relay.credential.Token)
		response, err := root.http.Client().Do(request)
		require.NoError(t, err)
		require.GreaterOrEqual(t, response.StatusCode, 400)
		require.NoError(t, response.Body.Close())
	}
	require.Empty(t, vault.Requests())
}

func TestRelayDocbankBrowseCancellationReachesOrigin(t *testing.T) {
	root, _, leaf, _ := newRelayClaimChain(t)
	started, canceled, stop := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	origin := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); origin.Close() })
	serveBrowseFixture(t, root, root.store, origin.URL, "/kata")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/docbank/children", leaf.http.URL, leaf.project.ID), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+leaf.userToken)
	done := make(chan error, 1)
	go func() {
		response, err := leaf.http.Client().Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("origin did not begin the synthetic read")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("leaf request did not retire")
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("origin context was not canceled")
	}
}

func TestRelayDocbankBrowseNativeNamesAndNoReadEvents(t *testing.T) {
	root, relay, leaf, _ := newRelayClaimChain(t)
	vault := docbanktest.New(t)
	vault.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
	names := []string{"résumé.pdf", "line\nbreak.pdf", "%2F", strings.Repeat("n", 300)}
	for i, name := range names {
		vault.Seed("/kata/"+name, docbank.Node{ID: int64(100 + i), Name: name, Kind: "file"}, nil)
	}
	serveRelayDocbank(t, root, vault)
	before := map[*relayMatrixNode]int64{}
	for _, node := range []*relayMatrixNode{root, relay, leaf} {
		id, err := node.store.MaxEventID(t.Context())
		require.NoError(t, err)
		before[node] = id
	}
	for _, name := range names {
		endpoint := fmt.Sprintf("/api/v1/projects/%d/docbank/resolve?%s", leaf.project.ID, url.Values{"path": {"/kata/" + name}}.Encode())
		status, raw := relayDocbankRequest(t, leaf, http.MethodGet, endpoint, "", "")
		require.Equal(t, http.StatusOK, status, string(raw))
		var result struct {
			Node struct {
				Name string `json:"name"`
				Path string `json:"path"`
			} `json:"node"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Equal(t, name, result.Node.Name)
		require.Equal(t, "/kata/"+name, result.Node.Path)
	}
	for _, node := range []*relayMatrixNode{root, relay, leaf} {
		id, err := node.store.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before[node], id)
	}
	status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/children?path=/kata/missing", leaf.project.ID), "", "")
	require.Equal(t, http.StatusNotFound, status, string(raw))
	for _, request := range vault.Requests() {
		require.True(t, strings.HasPrefix(request, "GET "), request)
	}
}

func TestRelayDocbankBrowseDisabledRouteNeverUsesLocalVault(t *testing.T) {
	_, _, leaf, _ := newRelayClaimChain(t)
	local := docbanktest.New(t)
	local.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
	serveRelayDocbank(t, leaf, local)
	binding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
	require.NoError(t, err)
	binding.Enabled = false
	_, err = leaf.store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	for _, operation := range []string{"children", "resolve?path=/kata"} {
		status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/%s", leaf.project.ID, operation), "", "")
		require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
	}
	require.Empty(t, local.Requests())
}

func TestRelayDocbankBrowseBindingWithdrawnDuringRead(t *testing.T) {
	for _, operation := range []string{"children", "resolve?path=/kata"} {
		t.Run(operation, func(t *testing.T) {
			root, _, leaf, _ := newRelayClaimChain(t)
			origin, local := docbanktest.New(t), docbanktest.New(t)
			origin.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
			local.Seed("/kata", docbank.Node{ID: 20, Name: "kata", Kind: "dir"}, nil)
			serveRelayDocbank(t, root, origin)
			serveRelayDocbank(t, leaf, local)
			binding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
			require.NoError(t, err)
			withdrawn := make(chan error, 1)
			var once sync.Once
			origin.BeforeResponse = func(_, _ string) {
				once.Do(func() {
					binding.Enabled = false
					_, err := leaf.store.UpsertFederationBinding(t.Context(), binding)
					withdrawn <- err
				})
			}
			status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/%s", leaf.project.ID, operation), "", "")
			select {
			case err := <-withdrawn:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("synthetic binding withdrawal did not run")
			}
			require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
			require.NotContains(t, string(raw), `"directory"`)
			require.NotContains(t, string(raw), `"node"`)
			require.Empty(t, local.Requests())
		})
	}
}
