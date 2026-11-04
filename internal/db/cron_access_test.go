package db

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

// Only a missing row has default authority. Present documents must be strict,
// versioned, canonical, and validated in both directions.
func TestDecodeCronAccessPolicy(t *testing.T) {
	project, err := uid.New()
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		revision := hegel.Draw(ht, hegel.Integers(1, math.MaxInt))
		raw := fmt.Sprintf(`{"version":1,"revision":%d,"define":{"mode":"any","team_uids":[]},"observe":{"mode":"teams","team_uids":[]}}`, revision)
		got, err := DecodeCronAccessPolicy(project, []byte(raw))
		require.NoError(ht, err)
		require.Equal(ht, int64(revision), got.Revision)
		require.Equal(ht, project, got.ProjectUID)
		require.Equal(ht, "any", got.Define.Mode)
		require.Equal(ht, "teams", got.Observe.Mode)
	}, hegel.WithDatabase(t.TempDir()))
	for _, raw := range []string{
		`null`, `{}`, `{"version":2,"revision":1,"define":{"mode":"any"},"observe":{"mode":"any"}}`,
		`{"version":1,"revision":0,"define":{"mode":"any"},"observe":{"mode":"any"}}`,
		`{"version":1,"revision":1,"define":{"mode":"unknown"},"observe":{"mode":"any"}}`,
		`{"version":1,"revision":1,"define":{"mode":"teams","team_uids":["invalid"]},"observe":{"mode":"any"}}`,
		`{"version":1,"revision":1,"define":{"mode":"any"},"observe":{"mode":"any"},"extra":true}`,
		`{"version":1,"version":1,"revision":1,"define":{"mode":"any"},"observe":{"mode":"any"}}`,
	} {
		_, err := DecodeCronAccessPolicy(project, []byte(raw))
		require.Error(t, err, raw)
	}
}

// Hegel shrank the positive-revision domain to MaxInt: reading the last valid
// revision must succeed; only advancing it is forbidden.
func TestDecodeCronAccessPolicyMaxRevision(t *testing.T) {
	project, err := uid.New()
	require.NoError(t, err)
	raw := fmt.Sprintf(`{"version":1,"revision":%d,"define":{"mode":"any"},"observe":{"mode":"any"}}`, int64(math.MaxInt64))
	got, err := DecodeCronAccessPolicy(project, []byte(raw))
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64), got.Revision)
}
