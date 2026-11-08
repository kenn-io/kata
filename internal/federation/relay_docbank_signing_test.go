package federation_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/metadata"
)

// Every fixture origin has an explicit in-memory transport. Unknown origins
// fail rather than falling through to the real network or a local service.
type docbankFixtureRoutes struct{ routes sync.Map }

func (r *docbankFixtureRoutes) RoundTrip(req *http.Request) (*http.Response, error) {
	transport, ok := r.routes.Load(req.URL.Host)
	if !ok {
		return nil, errors.New("unregistered fixture origin")
	}
	roundTripper, ok := transport.(http.RoundTripper)
	if !ok {
		return nil, errors.New("invalid fixture transport")
	}
	return roundTripper.RoundTrip(req)
}

func (r *docbankFixtureRoutes) replace(t *testing.T, old *httptest.Server, host string) *httptest.Server {
	t.Helper()
	handler := old.Config.Handler
	old.Close()
	server := httptest.NewTestServer(t, handler)
	transport := server.Client().Transport
	server.URL = "https://" + host
	r.routes.Store(host, transport)
	return server
}

// Compose the real public upload, native credential signer, relay, receiver
// verifier, root Docbank and immediate content route. Generic body wrappers
// must not turn an unknown client length into an exact empty body.
func TestRelayDocbankSignedUploadThroughBothHops(t *testing.T) {
	testRelayDocbankSignedTransfer(t, 0)
}

// A content response may arrive beyond the ordinary ten-second control budget.
func TestRelayDocbankSignedContentTransferBudget(t *testing.T) {
	testRelayDocbankSignedTransfer(t, 11*time.Second)
}

func testRelayDocbankSignedTransfer(t *testing.T, contentDelay time.Duration) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		routes := new(docbankFixtureRoutes)
		previous := http.DefaultTransport
		http.DefaultTransport = routes
		t.Cleanup(func() { http.DefaultTransport = previous })
		var next int
		root, relay, leaf, issue := newRelayClaimChainWithSetup(t, func(node *relayMatrixNode) {
			next++
			node.http = routes.replace(t, node.http, fmt.Sprintf("node%d.example.com", next))
		})
		vaults := make(map[*relayMatrixNode]*docbanktest.Fake)
		for i, node := range []*relayMatrixNode{root, relay, leaf} {
			vault := docbanktest.New(t)
			vault.Server = routes.replace(t, vault.Server, fmt.Sprintf("vault%d.example.com", i))
			vaults[node] = vault
		}
		t.Setenv("TEST_DOCBANK_ROOT_HOP_KEY", strings.Repeat("a", 64))
		t.Setenv("TEST_DOCBANK_RELAY_HOP_KEY", strings.Repeat("b", 64))
		verifiers := make(map[*relayMatrixNode]*federationsigning.Verifier)
		for _, hop := range []struct {
			upstream, downstream *relayMatrixNode
			env, id              string
		}{{root, relay, "TEST_DOCBANK_ROOT_HOP_KEY", "root-hop"}, {relay, leaf, "TEST_DOCBANK_RELAY_HOP_KEY", "relay-hop"}} {
			source := signingpolicy.Source{KeyID: hop.id, KeyEnv: hop.env, HubURL: hop.upstream.http.URL}
			enrollment, err := hop.upstream.store.AuthorizeFederationToken(t.Context(), hop.downstream.credential.Token, hop.upstream.project.ID, "push")
			require.NoError(t, err)
			state := filepath.Join(t.TempDir(), "replay.state")
			require.NoError(t, federationsigning.InitializeReplayState(state))
			verifier, err := federationsigning.NewVerifier(hop.upstream.http.URL, []signingpolicy.Key{{Source: source, EnrollmentID: enrollment.ID}}, state)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, verifier.Close()) })
			verifiers[hop.upstream] = verifier
			hop.downstream.credential.Signing = &source
			require.NoError(t, hop.downstream.credentials.StoreFederationCredential(t.Context(), hop.downstream.project.UID, hop.downstream.credential))
		}
		for _, node := range []*relayMatrixNode{root, relay, leaf} {
			server := daemon.NewServer(daemon.ServerConfig{DB: node.store, FederationCredentials: node.credentials, RootSigning: daemon.NewRootSigning(node.signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, FederationSigning: verifiers[node], FederationSigningRequired: verifiers[node] != nil, Docbank: docbank.NewClient(vaults[node].Server.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			handler := server.Handler()
			node.handler.Store(&handler)
		}
		// All networking is in memory; quarantine advances without real delay.
		time.Sleep(federationsigning.Quarantine)
		status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "cli")
		require.Equal(t, http.StatusOK, status)
		require.True(t, acquired.Granted)
		endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, issue.UID)
		status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=signed.pdf&client_kind=cli", "signed fixture bytes", "application/octet-stream")
		require.Equal(t, http.StatusOK, status, string(raw))
		var uploaded api.AddAttachmentResponseBody
		require.NoError(t, json.Unmarshal(raw, &uploaded))
		require.Equal(t, root.store.InstanceUID(), uploaded.Attachment.Origin)
		vaults[root].BeforeResponse = func(method, path string) {
			if method == http.MethodGet && strings.HasSuffix(path, "/content") {
				time.Sleep(contentDelay)
			}
		}
		readSignedDocbankContent(t, leaf, endpoint+"/"+uploaded.ID+"/content", "signed fixture bytes")
		// Empty content still needs a live authorized header release; io.Copy
		// makes no body Write in this case.
		emptyVersion := uuid.MustParse("6f1c2e8a-3333-4333-8333-333333333319")
		vaults[root].Seed("/kata/empty.pdf", docbank.Node{ID: 501, Name: "empty.pdf", Kind: "file", CurrentVersionID: emptyVersion}, map[uuid.UUID][]byte{emptyVersion: nil})
		emptyReference, err := json.Marshal(metadata.Attachment{Origin: root.store.InstanceUID(), Name: "empty.pdf", MediaType: "application/pdf", Size: 0, DocbankVersion: emptyVersion})
		require.NoError(t, err)
		_, err = leaf.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issueIDForRelayMatrix(t, leaf, issue.UID), Actor: leaf.account, Patch: map[string]jsontext.Value{"attachment.empty": emptyReference}})
		require.NoError(t, err)
		readSignedDocbankContent(t, leaf, endpoint+"/empty/content", "")
		require.NotEmpty(t, vaults[root].Requests())
		require.Empty(t, vaults[relay].Requests())
		require.Empty(t, vaults[leaf].Requests())
	})
}

func readSignedDocbankContent(t *testing.T, leaf *relayMatrixNode, endpoint, want string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, leaf.http.URL+endpoint, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+leaf.userToken)
	resp, err := leaf.http.Client().Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, want, string(body))
	digest := sha256.Sum256([]byte(want))
	require.Equal(t, hex.EncodeToString(digest[:]), resp.Header.Get("X-Docbank-Blob-Hash"))
	require.Equal(t, strconv.Itoa(len(want)), resp.Header.Get("X-Docbank-Blob-Size"))
	require.Empty(t, resp.Header.Get("Set-Cookie"), "Docbank response cookies must not cross the service boundary")
}
