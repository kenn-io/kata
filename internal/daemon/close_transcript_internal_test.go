package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

func TestCloseTranscript_AbsentPreservesLegacyRetryFingerprint(t *testing.T) {
	// A receipt issued before transcript support must still replay. Keep the
	// previous fingerprint document as the independent compatibility oracle.
	legacy := struct {
		IssueUID   string         `json:"issue_uid"`
		RequestRef string         `json:"request_ref"`
		Actor      string         `json:"actor"`
		Reason     string         `json:"reason"`
		Message    string         `json:"message"`
		Source     string         `json:"source"`
		Evidence   []api.Evidence `json:"evidence"`
		DryRun     bool           `json:"dry_run"`
		IfMatchRev *int64         `json:"if_match_revision"`
	}{IssueUID: "example-issue", RequestRef: "abc4", Actor: "worker", Reason: "done", Message: "Implemented the example behavior and ran the focused tests.", Evidence: []api.Evidence{{Type: api.EvidenceTest, Command: "go test ./internal/example"}}, IfMatchRev: new(int64(7))}
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	got := closeIdempotencyFingerprint(legacy.IssueUID, legacy.RequestRef, legacy.Actor, legacy.Reason, legacy.Message, legacy.Source, legacy.Evidence, legacy.DryRun, legacy.IfMatchRev, nil)
	require.Equal(t, hex.EncodeToString(sum[:]), got)
}
