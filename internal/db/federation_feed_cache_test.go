package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Two reads of one project that commit out of order keep the newest proof of
// each kind: the later quiet range and the higher move check.
func TestFederationFeedCacheKeepsNewestProofs(t *testing.T) {
	var cache FederationFeedCache
	project := Project{ID: 7, UID: "01M8000000000000000000000P", Revision: 1}
	binding := FederationBinding{}

	movesChecked := cache.Begin(project, binding)
	movesChecked.recordMoves(false)
	movesChecked.recordSnapshot(50)
	movesChecked.recordQuiet(0, 50)

	rangeChecked := cache.Begin(project, binding)
	rangeChecked.recordMoves(true)
	rangeChecked.recordSnapshot(60)
	rangeChecked.recordQuiet(0, 60)

	movesChecked.Commit()
	rangeChecked.Commit()

	next := cache.Begin(project, binding)
	require.Equal(t, int64(50), next.movesAfter(), "move check")
	after, ok := next.foreignChecked(0)
	require.True(t, ok)
	require.Equal(t, int64(60), after, "quiet range")
}

// A read that returned events still proves the move check for its snapshot.
func TestFederationFeedCacheRecordsMoveCheckWithoutQuietRange(t *testing.T) {
	var cache FederationFeedCache
	project := Project{ID: 7, UID: "01M8000000000000000000000P", Revision: 1}
	read := cache.Begin(project, FederationBinding{})
	read.recordMoves(false)
	read.recordSnapshot(80)
	read.Commit()
	require.Equal(t, int64(80), cache.Begin(project, FederationBinding{}).movesAfter())
}
