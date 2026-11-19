package vector

import (
	"testing"

	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
)

func TestPostgresQueryNonPositiveLimitDoesNotUseDatabase(t *testing.T) {
	ix := &Index{pg: &postgresIndex{}}
	for _, limit := range []int{0, -1} {
		hits, err := ix.Query(t.Context(), "generation", kitvec.Vector{1, 0}, limit)
		require.NoError(t, err)
		require.Empty(t, hits)
	}
}
