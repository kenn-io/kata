package federation_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/metadata"
)

type wrongContentHashResponse struct{ http.ResponseWriter }

func (w wrongContentHashResponse) Write(p []byte) (int, error) {
	w.Header().Set("X-Docbank-Blob-Hash", strings.Repeat("1", 64))
	return w.ResponseWriter.Write(p)
}

func TestRelayDocbankContentFailureSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, path, trashed, code string
		status                    int
		missing, emptyCorrupt     bool
	}{
		{name: "missing", path: "/kata/file.pdf", status: http.StatusGone, code: "attachment_content_missing", missing: true},
		{name: "trashed", path: "/kata/file.pdf", trashed: "2026-01-01T00:00:00Z", status: http.StatusGone, code: "attachment_content_missing"},
		{name: "moved outside", path: "/private/file.pdf", status: http.StatusForbidden, code: "docbank_outside_scope"},
		{name: "prefix sibling", path: "/kata-other/file.pdf", status: http.StatusForbidden, code: "docbank_outside_scope"},
		{name: "noncanonical", path: "/kata/../private/file.pdf", status: http.StatusBadGateway, code: "docbank_scope_unavailable"},
		{name: "historical inside", path: "/kata/file.pdf", status: http.StatusOK},
		{name: "empty corrupt", path: "/kata/file.pdf", emptyCorrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, leaf, issue := newRelayClaimChain(t)
			origin, local := docbanktest.New(t), docbanktest.New(t)
			version := uuid.MustParse("6f1c2e8a-5555-4555-8555-555555555519")
			latest := uuid.MustParse("6f1c2e8a-5555-4555-8555-555555555529")
			data := "origin document"
			if tc.emptyCorrupt {
				data = ""
				original := origin.Server.Config.Handler
				corrupted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/content") {
						w = wrongContentHashResponse{ResponseWriter: w}
					}
					original.ServeHTTP(w, r)
				}))
				t.Cleanup(corrupted.Close)
				origin.Server.Close()
				origin.Server = corrupted
			}
			if !tc.missing {
				origin.Seed(tc.path, docbank.Node{ID: 42, Name: "file.pdf", Kind: "file", TrashedAt: tc.trashed, CurrentVersionID: latest}, map[uuid.UUID][]byte{version: []byte(data), latest: []byte("latest document")})
			}
			local.Seed("/legacy/file.pdf", docbank.Node{ID: 99, Name: "file.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("local collision")})
			serveRelayDocbank(t, root, origin)
			serveRelayDocbank(t, leaf, local)
			raw, err := json.Marshal(metadata.Attachment{Origin: root.store.InstanceUID(), Name: "file.pdf", MediaType: "application/pdf", Size: int64(len(data)), DocbankVersion: version})
			require.NoError(t, err)
			_, err = leaf.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issueIDForRelayMatrix(t, leaf, issue.UID), Actor: leaf.account, Patch: map[string]jsontext.Value{"attachment.selected": raw}})
			require.NoError(t, err)
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/selected/content", leaf.project.ID, issue.UID)
			status, body := relayDocbankRequest(t, leaf, http.MethodGet, endpoint, "", "")
			if tc.emptyCorrupt {
				// A zero Content-Length consumer cannot detect missing bytes.
				// Integrity failure must prevent a successful empty response.
				require.NotEqual(t, http.StatusOK, status)
				var envelope api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(body, &envelope))
				require.NotEmpty(t, envelope.Error.Code)
				require.Empty(t, local.Requests())
				return
			}
			require.Equal(t, tc.status, status, string(body))
			if tc.status == http.StatusOK {
				require.Equal(t, data, string(body))
			} else {
				require.Contains(t, string(body), tc.code)
			}
			require.Empty(t, local.Requests(), "foreign-origin failure must not use the colliding local version")
		})
	}
}

func TestRelayDocbankContentLegacyMarkersRemainLocal(t *testing.T) {
	for _, origin := range []string{"", "local"} {
		t.Run("origin="+origin, func(t *testing.T) {
			root, _, leaf, issue := newRelayClaimChain(t)
			root.http.Close()
			local := docbanktest.New(t)
			version := uuid.MustParse("6f1c2e8a-6666-4666-8666-666666666619")
			local.Seed("/legacy/file.pdf", docbank.Node{ID: 99, Name: "file.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("legacy local bytes")})
			serveRelayDocbank(t, leaf, local)
			// Legacy reads preserve their reported-size behavior as well as local
			// routing, even while the project's configured root is unreachable.
			raw, err := json.Marshal(metadata.Attachment{Origin: origin, Name: "file.pdf", MediaType: "application/pdf", Size: 999, DocbankVersion: version})
			require.NoError(t, err)
			_, err = leaf.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issueIDForRelayMatrix(t, leaf, issue.UID), Actor: leaf.account, Patch: map[string]jsontext.Value{"attachment.legacy": raw}})
			require.NoError(t, err)
			status, body := relayDocbankRequest(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/legacy/content", leaf.project.ID, issue.UID), "", "")
			require.Equal(t, http.StatusOK, status, string(body))
			require.Equal(t, "legacy local bytes", string(body))
		})
	}
}
