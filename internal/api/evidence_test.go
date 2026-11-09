// Internal test for Evidence (un)marshal — placed in package api per the
// surrounding wire-types layout (Plan 1 §4) so future helpers that may need
// the unexported validEvidenceTypes map don't need a parallel test file.
//
//nolint:revive // package-name lint flagged externally; api is the fixed name.
package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidence_MarshalCommit(t *testing.T) {
	e := Evidence{Type: EvidenceCommit, SHA: "abc1234"}
	bs, err := json.Marshal(e)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"commit","sha":"abc1234"}`, string(bs))
}

func TestEvidence_UnmarshalReviewedPaths(t *testing.T) {
	in := `{"type":"reviewed-paths","paths":["a/b.go","c/d.go"]}`
	var e Evidence
	require.NoError(t, json.Unmarshal([]byte(in), &e))
	assert.Equal(t, EvidenceReviewedPaths, e.Type)
	assert.Equal(t, []string{"a/b.go", "c/d.go"}, e.Paths)
}

func TestEvidence_UnmarshalDuplicateOf(t *testing.T) {
	in := `{"type":"duplicate-of","issue_ref":"abc4"}`
	var e Evidence
	require.NoError(t, json.Unmarshal([]byte(in), &e))
	assert.Equal(t, EvidenceDuplicateOf, e.Type)
	assert.Equal(t, "abc4", e.IssueRef)
}

func TestEvidence_UnmarshalExternalPreservesAccount(t *testing.T) {
	in := `{"type":"external","account":"arranged by email; calendar hold sent"}`
	var e Evidence
	require.NoError(t, json.Unmarshal([]byte(in), &e))

	bs, err := json.Marshal(e)
	require.NoError(t, err)
	assert.JSONEq(t, in, string(bs))
}

func TestEvidence_UnmarshalUnknownTypeIsError(t *testing.T) {
	in := `{"type":"bogus"}`
	var e Evidence
	err := json.Unmarshal([]byte(in), &e)
	require.Error(t, err)
}

func TestEvidenceAttachmentWire(t *testing.T) {
	const input = `{"type":"attachment","attachment_id":"01K7E222222222222222222222"}`
	var e Evidence
	require.NoError(t, json.Unmarshal([]byte(input), &e))
	assert.Equal(t, EvidenceAttachment, e.Type)
	assert.Equal(t, "01K7E222222222222222222222", e.AttachmentID)
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	assert.JSONEq(t, input, string(raw))
	require.Error(t, json.Unmarshal([]byte(`{"type":"attachment","attachment_id":"01K7E222222222222222222222","attachment":{"name":"forged.pdf"}}`), &e), "callers cannot supply a frozen server descriptor")
}
