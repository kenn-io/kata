package federation_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/metadata"
)

// A reference and a matching upstream hash may still disagree on content size.
// Reject that disagreement before publishing response headers or any file bytes.
func TestRelayDocbankContentRejectsReferenceSizeMismatch(t *testing.T) {
	root, _, leaf, issue := newRelayClaimChain(t)
	vault := docbanktest.New(t)
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222229")
	data := []byte("origin fixture")
	vault.Seed("/kata/reference.pdf", docbank.Node{ID: 42, Name: "reference.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: data})
	serveRelayDocbank(t, root, vault)
	for _, size := range []int64{0, int64(len(data) - 1), int64(len(data) + 1)} {
		raw, err := json.Marshal(metadata.Attachment{Origin: root.store.InstanceUID(), Name: "reference.pdf", MediaType: "application/pdf", Size: size, DocbankVersion: version})
		require.NoError(t, err)
		_, err = leaf.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issueIDForRelayMatrix(t, leaf, issue.UID), Actor: leaf.account, Patch: map[string]jsontext.Value{"attachment.size": raw}})
		require.NoError(t, err)
		endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/size/content", leaf.project.ID, issue.UID)
		status, raw := relayDocbankRequest(t, leaf, http.MethodGet, endpoint, "", "")
		require.Equal(t, http.StatusBadGateway, status, string(raw))
		require.NotContains(t, string(raw), string(data))
	}
}
