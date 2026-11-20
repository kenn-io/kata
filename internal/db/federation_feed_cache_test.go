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

// A remembered count of zero is still a count, so an empty project is not
// recounted on every poll.
func TestFederationFeedCacheReusesZeroIssueCount(t *testing.T) {
	var cache FederationFeedCache
	project := Project{ID: 7, UID: "01M8000000000000000000000P", Revision: 1}
	read := cache.Begin(project, FederationBinding{})
	read.recordIssues(0)
	read.Commit()
	issues, ok := cache.Begin(project, FederationBinding{}).knownIssues()
	require.True(t, ok)
	require.Zero(t, issues)
}

// Every change to which issues a project owns reaches its feed as an event,
// so a read that reused the remembered count and returned events drops it.
// A count the read took itself stays.
func TestFederationFeedCacheForgetsReusedIssueCountAfterEvents(t *testing.T) {
	var cache FederationFeedCache
	project := Project{ID: 7, UID: "01M8000000000000000000000P", Revision: 1}
	counting := cache.Begin(project, FederationBinding{})
	counting.recordSnapshot(40)
	counting.recordIssues(40)
	counting.recordEvents()
	counting.Commit()
	issues, ok := cache.Begin(project, FederationBinding{}).knownIssues()
	require.True(t, ok)
	require.Equal(t, int64(40), issues)

	reusing := cache.Begin(project, FederationBinding{})
	reusing.recordSnapshot(50)
	reusing.recordEvents()
	reusing.Commit()
	_, ok = cache.Begin(project, FederationBinding{}).knownIssues()
	require.False(t, ok)
}

// A quiet poll that read an older snapshot and commits last must not restore
// a count that a newer read already found stale.
func TestFederationFeedCacheKeepsNewerRecountAfterOlderQuietPoll(t *testing.T) {
	var cache FederationFeedCache
	project := Project{ID: 7, UID: "01M8000000000000000000000P", Revision: 1}
	warm := cache.Begin(project, FederationBinding{})
	warm.recordSnapshot(10)
	warm.recordIssues(500)
	warm.Commit()

	quiet := cache.Begin(project, FederationBinding{})
	quiet.recordSnapshot(20)
	quiet.recordQuiet(0, 20)

	moved := cache.Begin(project, FederationBinding{})
	moved.recordSnapshot(30)
	moved.recordEvents()
	moved.Commit()

	quiet.Commit()
	_, ok := cache.Begin(project, FederationBinding{}).knownIssues()
	require.False(t, ok, "the older quiet poll restored a stale count")
	after, ok := cache.Begin(project, FederationBinding{}).foreignChecked(0)
	require.True(t, ok)
	require.Equal(t, int64(20), after, "quiet range")
}
