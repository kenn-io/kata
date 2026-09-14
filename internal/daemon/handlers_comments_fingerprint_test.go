package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommentTeammateFingerprintPreservesLegacy(t *testing.T) {
	old, err := json.Marshal(struct {
		IssueUID string `json:"issue_uid"`
		Actor    string `json:"actor"`
		Body     string `json:"body"`
	}{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "coordinator", "check retries"})
	require.NoError(t, err)
	sum := sha256.Sum256(old)
	require.Equal(t, hex.EncodeToString(sum[:]), commentIdempotencyFingerprint("01ARZ3NDEKTSV4RRFFQ69G5FAV", "coordinator", "check retries", ""))
	require.NotEqual(t, hex.EncodeToString(sum[:]), commentIdempotencyFingerprint("01ARZ3NDEKTSV4RRFFQ69G5FAV", "coordinator", "check retries", "reviewer-7"))
}
