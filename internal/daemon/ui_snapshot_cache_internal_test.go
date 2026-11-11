package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestUISnapshotCacheUsesWallClockCreation(t *testing.T) {
	cache := newUISnapshotAuthorityCache()
	cache.put("wall-clock", db.UISnapshotData{}, nil)
	createdAt := cache.entries["wall-clock"].createdAt
	require.Equal(t, createdAt.Round(0), createdAt, "rollback validation must compare wall clocks even when the default clock includes monotonic time")
}
