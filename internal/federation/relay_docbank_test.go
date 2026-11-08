package federation_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/metadata"
)

func serveRelayDocbank(t *testing.T, node *relayMatrixNode, f *docbanktest.Fake) {
	t.Helper()
	server := daemon.NewServer(daemon.ServerConfig{DB: node.store, FederationCredentials: node.credentials,
		RootSigning: daemon.NewRootSigning(node.signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true},
		Docbank: docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler := server.Handler()
	node.handler.Store(&handler)
}

func relayDocbankRequest(t *testing.T, node *relayMatrixNode, method, endpoint, body, typ string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, node.http.URL+endpoint, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+node.userToken)
	if typ != "" {
		req.Header.Set("Content-Type", typ)
	}
	resp, err := node.http.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// The initiating replica must get an immediately usable reference while only
// the root's Docbank receives bytes. No metadata sync is needed for content.
func TestRelayDocbankUploadAndImmediateContent(t *testing.T) {
	root, relay, leaf, issue := newRelayClaimChain(t)
	rootVault, relayVault, leafVault := docbanktest.New(t), docbanktest.New(t), docbanktest.New(t)
	serveRelayDocbank(t, root, rootVault)
	serveRelayDocbank(t, relay, relayVault)
	serveRelayDocbank(t, leaf, leafVault)
	code, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "")
	require.Equal(t, http.StatusOK, code)
	require.True(t, acquired.Granted)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "root document", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
	var out api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, root.store.InstanceUID(), out.Attachment.Origin)
	local, err := leaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Contains(t, local.Metadata, out.ID)
	status, raw = relayDocbankRequest(t, leaf, http.MethodGet, endpoint+"/"+out.ID+"/content", "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, "root document", string(raw))
	require.Empty(t, relayVault.Requests())
	require.Empty(t, leafVault.Requests())
	syncRelayMatrixNode(t, leaf)
	syncRelayMatrixNode(t, relay)
	syncRelayMatrixNode(t, leaf)
	for _, node := range []*relayMatrixNode{root, relay, leaf} {
		stored, err := node.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		var values map[string]metadata.Attachment
		require.NoError(t, json.Unmarshal([]byte(stored.Metadata), &values))
		require.Equal(t, out.Attachment, values[metadata.AttachmentPrefix+out.ID])
	}
}

func TestRelayDocbankPendingIssueAndOfflineNeverUseLocalVault(t *testing.T) {
	root, relay, leaf, issue := newRelayClaimChain(t)
	rootVault, localVault := docbanktest.New(t), docbanktest.New(t)
	serveRelayDocbank(t, root, rootVault)
	serveRelayDocbank(t, leaf, localVault)
	pending, _, err := leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Author: leaf.account, Title: "pending attachment issue"})
	require.NoError(t, err)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, pending.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "pending document", "application/octet-stream")
	require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
	require.Contains(t, string(raw), "attachment_issue_not_synced")
	require.Empty(t, rootVault.Requests())
	require.Empty(t, localVault.Requests())
	// Replication makes the issue known; the retry now succeeds at the origin.
	syncRelayMatrixNode(t, leaf)
	syncRelayMatrixNode(t, relay)
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "pending document", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
	var out api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	root.http.Close()
	status, raw = relayDocbankRequest(t, leaf, http.MethodGet, endpoint+"/"+out.ID+"/content", "", "")
	require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
	require.Contains(t, string(raw), "federation_offline")
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "offline document", "application/octet-stream")
	require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
	require.Empty(t, localVault.Requests())
	// Historical unmarked references still use the serving daemon's vault,
	// even for this adopted relay project with an unreachable root.
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222228")
	localVault.Seed("/legacy/file.pdf", docbank.Node{ID: 88, Name: "file.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("legacy bytes")})
	ref, err := json.Marshal(metadata.Attachment{Name: "file.pdf", MediaType: "application/pdf", Size: 12, DocbankVersion: version})
	require.NoError(t, err)
	_, err = leaf.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issueIDForRelayMatrix(t, leaf, issue.UID), Actor: leaf.account, Patch: map[string]jsontext.Value{"attachment.legacy": ref}})
	require.NoError(t, err)
	status, raw = relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/legacy/content", leaf.project.ID, issue.UID), "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, "legacy bytes", string(raw))
}

func TestRelayDocbankLinkUsesRootAndRefusesSubtreeEscape(t *testing.T) {
	root, relay, leaf, issue := newRelayClaimChain(t)
	f := docbanktest.New(t)
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222")
	f.Seed("/kata/report.pdf", docbank.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("root document")})
	f.Seed("/private/report.pdf", docbank.Node{ID: 43, Name: "report.pdf", Kind: "file", CurrentVersionID: version}, nil)
	serveRelayDocbank(t, root, f)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint, `{"path":"/kata/report.pdf","reuse_existing":true}`, "application/json")
	require.Equal(t, http.StatusOK, status, string(raw))
	var first, second api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &first))
	require.Equal(t, root.store.InstanceUID(), first.Attachment.Origin)
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint, `{"node_id":42,"reuse_existing":true}`, "application/json")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &second))
	require.Equal(t, first.ID, second.ID)
	for _, body := range []string{`{"path":"/private/report.pdf"}`, `{"node_id":43}`} {
		status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint, body, "application/json")
		require.Equal(t, http.StatusForbidden, status, string(raw))
	}
	_ = relay
}

// A CLI lease includes its client kind before either relay hashes the tuple.
func TestRelayDocbankCLILeaseHolderCanUploadAndLink(t *testing.T) {
	root, _, leaf, issue := newRelayClaimChain(t)
	vault := docbanktest.New(t)
	serveRelayDocbank(t, root, vault)
	status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "cli")
	require.Equal(t, http.StatusOK, status)
	require.True(t, acquired.Granted)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=file.pdf&client_kind=cli", "CLI fixture", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
	var uploaded api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &uploaded))
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222221")
	vault.Seed("/kata/existing.pdf", docbank.Node{ID: 99, Name: "existing.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("linked fixture")})
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"/links", `{"path":"/kata/existing.pdf","client_kind":"cli"}`, "application/json")
	require.Equal(t, http.StatusOK, status, string(raw))
	var linked api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &linked))
	require.Equal(t, root.store.InstanceUID(), linked.Attachment.Origin)
	require.Equal(t, version, linked.Attachment.DocbankVersion)
}

// Ordinary local mutation authority belongs to holder+instance across client
// labels; only the initiating daemon may recover that held lease's exact tuple.
func TestRelayDocbankLeaseSurvivesClientSwitch(t *testing.T) {
	for _, pair := range []struct{ lease, request string }{{"cli", ""}, {"", "cli"}} {
		t.Run(pair.lease+"-to-"+pair.request, func(t *testing.T) {
			root, _, leaf, issue := newRelayClaimChain(t)
			vault := docbanktest.New(t)
			serveRelayDocbank(t, root, vault)
			status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", pair.lease)
			require.Equal(t, http.StatusOK, status)
			require.True(t, acquired.Granted)
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, issue.UID)
			status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=file.pdf&client_kind="+pair.request, "switched fixture", "application/octet-stream")
			require.Equal(t, http.StatusOK, status, string(raw))
			version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222221")
			vault.Seed("/kata/existing.pdf", docbank.Node{ID: 99, Name: "existing.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("linked fixture")})
			status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"/links", fmt.Sprintf(`{"path":"/kata/existing.pdf","client_kind":%q}`, pair.request), "application/json")
			require.Equal(t, http.StatusOK, status, string(raw))
		})
	}
}
