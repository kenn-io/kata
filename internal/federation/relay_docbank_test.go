package federation_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/metadata"
)

func TestRelayDocbankReadAfterRootNewInstanceRestore(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			root, relay, leaf, issue := newRelayClaimChain(t)
			vault := docbanktest.New(t)
			serveRelayDocbank(t, root, vault)
			serveRelayDocbank(t, relay, nil)
			serveRelayDocbank(t, leaf, nil)
			node := leaf
			if direct {
				node = relay
			}
			code, acquired := relayClaimAction(t, node, issue.UID, "acquire", "")
			require.Equal(t, http.StatusOK, code)
			require.True(t, acquired.Granted)
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", node.project.ID, issue.UID)
			status, raw := relayDocbankRequest(t, node, http.MethodPost, endpoint+"?name=report.pdf", "root document", "application/octet-stream")
			require.Equal(t, http.StatusOK, status, string(raw))
			var out api.AddAttachmentResponseBody
			require.NoError(t, json.Unmarshal(raw, &out))
			foreign := out.Attachment
			foreign.Origin = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
			foreignRaw, err := json.Marshal(foreign)
			require.NoError(t, err)
			localIssue, err := node.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = node.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: localIssue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment.foreign": foreignRaw}})
			require.NoError(t, err)
			var backup bytes.Buffer
			require.NoError(t, jsonl.Export(t.Context(), root.store, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
			restored := openRelayMatrixStore(t, "sqlite", false)
			require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(backup.Bytes()), restored, jsonl.ImportOptions{NewInstance: true}))
			root.store = restored
			root.signer.AuthorityUID = restored.InstanceUID()
			serveRelayDocbank(t, root, vault)
			status, raw = relayDocbankRequest(t, node, http.MethodGet, endpoint+"/"+out.ID+"/content", "", "")
			require.Equal(t, http.StatusOK, status, string(raw))
			require.Equal(t, "root document", string(raw))
			status, raw = relayDocbankRequest(t, node, http.MethodGet, endpoint+"/foreign/content", "", "")
			require.Equal(t, http.StatusConflict, status, string(raw))
		})
	}
}

func serveRelayDocbank(t *testing.T, node *relayMatrixNode, f *docbanktest.Fake, syncOverride ...func(context.Context, db.FederationBinding, config.FederationCredential) error) {
	t.Helper()
	cfg := daemon.ServerConfig{FederationSyncOnce: func(ctx context.Context, binding db.FederationBinding, credential config.FederationCredential) error {
		return federation.SyncFederationOnce(ctx, node.store, binding, credential, node.credentials)
	}, DB: node.store, FederationCredentials: node.credentials,
		RootSigning: daemon.NewRootSigning(node.signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, DocbankFolder: "/kata"}
	if len(syncOverride) > 0 {
		cfg.FederationSyncOnce = syncOverride[0]
	}
	if f != nil {
		cfg.Docbank = docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	}
	server := daemon.NewServer(cfg)
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

func TestRelayDocbankUploadAndImmediateContent(t *testing.T) {
	root, relay, leaf, issue := newRelayClaimChain(t)
	rootVault := docbanktest.New(t)
	serveRelayDocbank(t, root, rootVault)
	serveRelayDocbank(t, relay, nil)
	serveRelayDocbank(t, leaf, nil)
	var rootPreflights, relayPreflights, relayClaimLookups atomic.Int32
	for _, entry := range []struct {
		node  *relayMatrixNode
		count *atomic.Int32
	}{{root, &rootPreflights}, {relay, &relayPreflights}} {
		previous := *entry.node.handler.Load()
		var wrapped http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/federation/docbank/capabilities") {
				entry.count.Add(1)
			}
			if entry.node == relay && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/lease") {
				relayClaimLookups.Add(1)
			}
			previous.ServeHTTP(w, r)
		})
		entry.node.handler.Store(&wrapped)
	}
	code, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "")
	require.Equal(t, http.StatusOK, code)
	require.True(t, acquired.Granted)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "root document", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.EqualValues(t, 2, relayClaimLookups.Load(), "one claim refresh before and one after upload")
	var out api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, root.store.InstanceUID(), out.Attachment.Origin)
	require.EqualValues(t, 1, rootPreflights.Load())
	require.EqualValues(t, 1, relayPreflights.Load())
	local, err := leaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Contains(t, local.Metadata, out.ID)
	status, raw = relayDocbankRequest(t, leaf, http.MethodGet, endpoint+"/"+out.ID+"/content", "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, "root document", string(raw))
	require.EqualValues(t, 2, rootPreflights.Load())
	require.EqualValues(t, 2, relayPreflights.Load())
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=second.pdf", "second document", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.EqualValues(t, 3, rootPreflights.Load())
	require.EqualValues(t, 3, relayPreflights.Load())
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222")
	rootVault.Seed("/kata/link.pdf", docbank.Node{ID: 42, Name: "link.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("linked document")})
	relayClaimLookups.Store(0)
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"/links", `{"path":"/kata/link.pdf"}`, "application/json")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.EqualValues(t, 2, relayClaimLookups.Load(), "one claim refresh before and one after linking")
	require.EqualValues(t, 4, rootPreflights.Load())
	require.EqualValues(t, 4, relayPreflights.Load())
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

func TestRelayDocbankContentAfterDetach(t *testing.T) {
	root, relay, leaf, issue := newRelayClaimChain(t)
	vault := docbanktest.New(t)
	serveRelayDocbank(t, root, vault)
	serveRelayDocbank(t, relay, nil)
	serveRelayDocbank(t, leaf, nil)
	status, claim := relayClaimAction(t, leaf, issue.UID, "acquire", "")
	require.Equal(t, http.StatusOK, status)
	require.True(t, claim.Granted)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "root document", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
	var out api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	status, raw = relayDocbankRequest(t, leaf, http.MethodGet, endpoint+"/"+out.ID+"/content", "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, "root document", string(raw))
	detacher, ok := leaf.store.(db.RelayLocalDetacher)
	require.True(t, ok)
	_, err := detacher.DetachRelayReplicaLocally(t.Context(), leaf.project.ID)
	require.NoError(t, err)
	status, raw = relayDocbankRequest(t, leaf, http.MethodGet, endpoint+"/"+out.ID+"/content", "", "")
	require.Equal(t, http.StatusGone, status, string(raw))
	require.Contains(t, string(raw), "attachment_origin_unavailable")
}

func TestRelayDocbankOfflineNeverUsesLocalVault(t *testing.T) {
	root, _, leaf, issue := newRelayClaimChain(t)
	rootVault, localVault := docbanktest.New(t), docbanktest.New(t)
	serveRelayDocbank(t, root, rootVault)
	serveRelayDocbank(t, leaf, localVault)
	pending, _, err := leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Author: leaf.account, Title: "pending attachment issue"})
	require.NoError(t, err)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, pending.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "pending document", "application/octet-stream")
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
	localVault := docbanktest.New(t)
	localVault.Seed("/local/report.pdf", docbank.Node{ID: 99, Name: "report.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("local document")})
	serveRelayDocbank(t, leaf, localVault)
	legacy, err := json.Marshal(metadata.Attachment{Name: "report.pdf", MediaType: "application/pdf", Size: 14, DocbankVersion: version})
	require.NoError(t, err)
	_, err = leaf.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issueIDForRelayMatrix(t, leaf, issue.UID), Actor: leaf.account, Patch: map[string]jsontext.Value{"attachment.legacy": legacy}})
	require.NoError(t, err)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint, `{"path":"/kata/report.pdf","reuse_existing":true}`, "application/json")
	require.Equal(t, http.StatusOK, status, string(raw))
	var first, second api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &first))
	require.Equal(t, root.store.InstanceUID(), first.Attachment.Origin)
	require.NotEqual(t, "legacy", first.ID)
	contentEndpoint := strings.TrimSuffix(endpoint, "/links")
	status, raw = relayDocbankRequest(t, leaf, http.MethodGet, contentEndpoint+"/legacy/content", "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, "local document", string(raw))
	status, raw = relayDocbankRequest(t, leaf, http.MethodGet, contentEndpoint+"/"+first.ID+"/content", "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, "root document", string(raw))
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint, `{"node_id":42,"reuse_existing":true}`, "application/json")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &second))
	require.Equal(t, first.ID, second.ID)
	for _, body := range []string{`{"path":"/private/report.pdf"}`, `{"node_id":43}`} {
		status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint, body, "application/json")
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), "move the file into the configured folder")
	}
	previous := *root.handler.Load()
	var denied http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/links") {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"claim_denied","message":"private upstream detail"}}`)
			return
		}
		previous.ServeHTTP(w, r)
	})
	root.handler.Store(&denied)
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint, `{"node_id":42}`, "application/json")
	require.Equal(t, http.StatusConflict, status, string(raw))
	require.Contains(t, string(raw), "run kata federation lease acquire")
	require.NotContains(t, string(raw), "private upstream detail")
	_ = relay
}

// Ordinary local mutation authority belongs to holder+instance across client
// labels on the initiating daemon.
func TestRelayDocbankLeaseSurvivesClientSwitch(t *testing.T) {
	root, _, leaf, issue := newRelayClaimChain(t)
	vault := docbanktest.New(t)
	serveRelayDocbank(t, root, vault)
	status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "cli")
	require.Equal(t, http.StatusOK, status)
	require.True(t, acquired.Granted)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=file.pdf", "switched fixture", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222221")
	vault.Seed("/kata/existing.pdf", docbank.Node{ID: 99, Name: "existing.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("linked fixture")})
	status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"/links", `{"path":"/kata/existing.pdf"}`, "application/json")
	require.Equal(t, http.StatusOK, status, string(raw))
	var linked api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &linked))
	require.Equal(t, root.store.InstanceUID(), linked.Attachment.Origin)
	require.Equal(t, version, linked.Attachment.DocbankVersion)
}

type unreadDocbankUpload struct{ reads int }

func (b *unreadDocbankUpload) Read([]byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func TestRelayDocbankUnsupportedPreflightLeavesUploadUnread(t *testing.T) {
	root, relay, leaf, issue := newRelayClaimChain(t)
	serveRelayDocbank(t, root, docbanktest.New(t))
	serveRelayDocbank(t, relay, nil)
	serveRelayDocbank(t, leaf, nil)
	status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "")
	require.Equal(t, http.StatusOK, status)
	require.True(t, acquired.Granted)
	previous := *relay.handler.Load()
	var unsupported http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/federation/docbank/capabilities") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found"}}`)
			return
		}
		previous.ServeHTTP(w, r)
	})
	relay.handler.Store(&unsupported)
	body := &unreadDocbankUpload{}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments?name=file.pdf", leaf.project.ID, issue.UID), body)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+leaf.userToken)
	response := httptest.NewRecorder()
	(*leaf.handler.Load()).ServeHTTP(response, req)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "docbank_forward_unsupported")
	require.Zero(t, body.reads)
}

func TestRelayDocbankReadRejectsUnsupportedCapabilities(t *testing.T) {
	for _, incompatible := range []string{"relay", "root"} {
		t.Run(incompatible, func(t *testing.T) {
			root, relay, leaf, issue := newRelayClaimChain(t)
			serveRelayDocbank(t, root, docbanktest.New(t))
			serveRelayDocbank(t, relay, nil)
			serveRelayDocbank(t, leaf, nil)
			status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "")
			require.Equal(t, http.StatusOK, status)
			require.True(t, acquired.Granted)
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, issue.UID)
			status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "root document", "application/octet-stream")
			require.Equal(t, http.StatusOK, status, string(raw))
			var out api.AddAttachmentResponseBody
			require.NoError(t, json.Unmarshal(raw, &out))
			node := root
			if incompatible == "relay" {
				node = relay
			}
			previous := *node.handler.Load()
			var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/federation/docbank/capabilities") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"version":2,"project_uid":%q,"origin":%q}`, node.project.UID, root.store.InstanceUID())
					return
				}
				previous.ServeHTTP(w, r)
			})
			node.handler.Store(&handler)
			status, raw = relayDocbankRequest(t, leaf, http.MethodGet, endpoint+"/"+out.ID+"/content", "", "")
			require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
			require.Contains(t, string(raw), "docbank_forward_unsupported")
		})
	}
}

func TestRelayDocbankNewIssueCanAttachImmediately(t *testing.T) {
	for _, operation := range []string{"upload", "link"} {
		t.Run(operation, func(t *testing.T) {
			root, _, leaf, _ := newRelayClaimChain(t)
			vault := docbanktest.New(t)
			serveRelayDocbank(t, root, vault)
			pending, _, err := leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Author: leaf.account, Title: "new shared issue"})
			require.NoError(t, err)
			if operation == "link" {
				syncRelayMatrixNode(t, leaf)
			}
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", leaf.project.ID, pending.UID)
			var status int
			var raw []byte
			if operation == "upload" {
				status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"?name=report.pdf", "new report", "application/octet-stream")
			} else {
				version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222")
				vault.Seed("/kata/report.pdf", docbank.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("linked report")})
				status, raw = relayDocbankRequest(t, leaf, http.MethodPost, endpoint+"/links", `{"path":"/kata/report.pdf"}`, "application/json")
			}
			require.Equal(t, http.StatusOK, status, string(raw))
			var out api.AddAttachmentResponseBody
			require.NoError(t, json.Unmarshal(raw, &out))
			require.Equal(t, root.store.InstanceUID(), out.Attachment.Origin)
			status, raw = relayDocbankRequest(t, leaf, http.MethodGet, endpoint+"/"+out.ID+"/content", "", "")
			require.Equal(t, http.StatusOK, status, string(raw))
			if operation == "upload" {
				require.Equal(t, "new report", string(raw))
			} else {
				require.Equal(t, "linked report", string(raw))
			}
			closeBody := fmt.Sprintf(`{"reason":"done","message":"Delivered and checked the new report for this issue.","evidence":[{"type":"attachment","attachment_id":%q}]}`, out.ID)
			status, raw = relayDocbankRequest(t, leaf, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", leaf.project.ID, pending.UID), closeBody, "application/json")
			require.Equal(t, http.StatusOK, status, string(raw))
		})
	}
}

func TestRelayDocbankNewIssueSyncFailureAndRebind(t *testing.T) {
	for _, mode := range []string{"failure", "rebind"} {
		t.Run(mode, func(t *testing.T) {
			root, _, leaf, _ := newRelayClaimChain(t)
			vault := docbanktest.New(t)
			serveRelayDocbank(t, root, vault)
			serveRelayDocbank(t, leaf, nil, func(ctx context.Context, binding db.FederationBinding, credential config.FederationCredential) error {
				if mode == "failure" {
					return errors.New("fixture sync failure")
				}
				if err := federation.SyncFederationOnce(ctx, leaf.store, binding, credential, leaf.credentials); err != nil {
					return err
				}
				_, err := leaf.store.RebindFederationBinding(ctx, db.RebindFederationBindingParams{ProjectID: binding.ProjectID, ExpectedHubURL: binding.HubURL, ExpectedAllowInsecure: binding.AllowInsecure, HubProjectID: binding.HubProjectID, HubProjectUID: binding.HubProjectUID, TargetHubURL: "http://127.0.0.1:1"})
				return err
			})
			issue, _, err := leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Author: leaf.account, Title: "pending issue"})
			require.NoError(t, err)
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments?name=report.pdf", leaf.project.ID, issue.UID)
			status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint, "fixture bytes", "application/octet-stream")
			require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
			require.Empty(t, vault.Requests())
			stored, err := leaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.JSONEq(t, "{}", string(stored.Metadata))
		})
	}
}

func TestRelayDocbankNewIssueAcrossTwoRelays(t *testing.T) {
	root := newRelayMatrixNode(t, "sqlite", "root-member")
	project, err := root.store.CreateProject(t.Context(), "shared")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	upper := newRelayMatrixNode(t, "sqlite", "upper-member")
	lower := newRelayMatrixNode(t, "sqlite", "lower-member")
	leaf := newRelayMatrixNode(t, "sqlite", "leaf-member")
	enrollRelayMatrixReplica(t, root, upper, "upper", true)
	syncRelayMatrixNode(t, upper)
	enrollRelayMatrixReplica(t, upper, lower, "lower", true)
	syncRelayMatrixNode(t, lower)
	enrollRelayMatrixReplica(t, lower, leaf, "leaf", false)
	syncRelayMatrixNode(t, leaf)
	vault := docbanktest.New(t)
	serveRelayDocbank(t, root, vault)
	issue, _, err := leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Author: leaf.account, Title: "new issue"})
	require.NoError(t, err)
	syncRelayMatrixNode(t, leaf)
	syncRelayMatrixNode(t, lower)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments?name=report.pdf", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint, "report", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
}

func TestRelayDocbankNewIssueAfterClaimRefreshTimeout(t *testing.T) {
	root, relay, leaf, _ := newRelayClaimChain(t)
	serveRelayDocbank(t, root, docbanktest.New(t))
	var calls atomic.Int32
	previous := *relay.handler.Load()
	var delayed http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/lease") && calls.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		previous.ServeHTTP(w, r)
	})
	relay.handler.Store(&delayed)
	issue, _, err := leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Author: leaf.account, Title: "new issue"})
	require.NoError(t, err)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments?name=report.pdf", leaf.project.ID, issue.UID)
	status, raw := relayDocbankRequest(t, leaf, http.MethodPost, endpoint, "report", "application/octet-stream")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.GreaterOrEqual(t, calls.Load(), int32(2))
}
