package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestFederationVectorRecordsDefersPastByteBudget(t *testing.T) {
	hash := func(s string) string { return vector.ContentSHA256(s) }
	stored := map[string]vector.StoredVectors{
		"a": {Doc: "a", ContentSHA256: hash("a"), Chunks: []kitvec.ChunkVector{{Vector: kitvec.Vector{1, 0}}}},
		"b": {Doc: "b", ContentSHA256: hash("b"), Chunks: []kitvec.ChunkVector{{Vector: kitvec.Vector{1, 0}}}},
		"c": {Doc: "c", ContentSHA256: hash("c")},
	}
	docs := []api.FederationVectorLookupDoc{
		{IssueUID: "a", ContentSHA256: hash("a")},
		{IssueUID: "b", ContentSHA256: hash("b")},
		{IssueUID: "c", ContentSHA256: hash("c")},
		{IssueUID: "d", ContentSHA256: hash("d")},
	}
	// Budget of 8 bytes fits exactly one 2-dim float32 vector.
	got := federationVectorRecords(docs, stored, 8)
	require.Len(t, got, 4)
	assert.Equal(t, api.FederationVectorStatusOK, got[0].Status)
	assert.Equal(t, api.FederationVectorStatusDeferred, got[1].Status)
	assert.Empty(t, got[1].Chunks)
	assert.Equal(t, api.FederationVectorStatusSkipped, got[2].Status)
	assert.Equal(t, api.FederationVectorStatusNotReady, got[3].Status)

	// The first ok record is always served, even when it alone exceeds the budget.
	first := federationVectorRecords(docs[:1], stored, 1)
	assert.Equal(t, api.FederationVectorStatusOK, first[0].Status)
}
