package federation_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/metadata"
)

// Correct body headers and a matching local reference still cannot override
// the immutable version's catalog size at the origin.
func TestRelayDocbankContentRejectsCatalogSizeMismatch(t *testing.T) {
	root, _, leaf, issue := newRelayClaimChain(t)
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222218")
	const data = "catalog fixture"
	sum := sha256.Sum256([]byte(data))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			http.Error(w, "fixture auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/versions/" + version.String() + "/content":
			w.Header().Set("X-Docbank-Blob-Hash", hex.EncodeToString(sum[:]))
			w.Header().Set("X-Docbank-Blob-Size", strconv.Itoa(len(data)))
			_, _ = io.WriteString(w, data)
		case "/api/v1/versions/" + version.String():
			_ = json.MarshalWrite(w, docbank.Version{ID: version, NodeID: 42, Size: int64(len(data) + 1)})
		case "/api/v1/nodes/42":
			_ = json.MarshalWrite(w, docbank.Node{ID: 42, Path: "/kata/file.pdf", Name: "file.pdf", Kind: "file", Size: int64(len(data) + 1), CurrentVersionID: version})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	server := daemon.NewServer(daemon.ServerConfig{DB: root.store, FederationCredentials: root.credentials, RootSigning: daemon.NewRootSigning(root.signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, Docbank: docbank.NewClient(upstream.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler := server.Handler()
	root.handler.Store(&handler)
	raw, err := json.Marshal(metadata.Attachment{Origin: root.store.InstanceUID(), Name: "file.pdf", MediaType: "application/pdf", Size: int64(len(data)), DocbankVersion: version})
	require.NoError(t, err)
	_, err = leaf.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issueIDForRelayMatrix(t, leaf, issue.UID), Actor: leaf.account, Patch: map[string]jsontext.Value{"attachment.catalog": raw}})
	require.NoError(t, err)
	status, raw := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/catalog/content", leaf.project.ID, issue.UID), "", "")
	require.Equal(t, http.StatusBadGateway, status, string(raw))
	require.NotContains(t, string(raw), data)
}
