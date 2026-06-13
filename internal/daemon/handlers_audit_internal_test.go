package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAuditParentFilterMatches_UIDAuthoritativeForModernRows pins the
// cross-project collision rule: short_ids are only unique within one
// project, so when the --parent filter resolved a concrete parent (UID
// known) and the row froze a parent UID at close time, UID equality is
// authoritative — the short_id fallbacks must not let a same-suffix
// parent in another project claim the row. Short_id matching remains for
// legacy rows (no frozen UID) and for purged parents (nothing resolved).
func TestAuditParentFilterMatches_UIDAuthoritativeForModernRows(t *testing.T) {
	const (
		localParentUID   = "01TESTPARENTLOCALAAAAAAAAA"
		foreignParentUID = "01TESTPARENTFOREIGNAAAAAAA"
	)
	localUID := localParentUID
	foreignUID := foreignParentUID
	emptyUID := ""

	// Bare same-project filter: resolved to the local parent "abc4".
	local := auditParentFilter{
		resolvedUID:     localParentUID,
		resolvedShortID: "abc4",
		parsedShortID:   "abc4",
		raw:             "abc4",
		has:             true,
	}
	assert.True(t, local.matches("abc4", &localUID),
		"modern row under the resolved parent must match")
	assert.False(t, local.matches("abc4", &foreignUID),
		"modern row under a same-suffix FOREIGN parent must not match by short_id")
	assert.True(t, local.matches("abc4", nil),
		"legacy row (no frozen UID) keeps the short_id fallback")
	assert.False(t, local.matches("", &emptyUID),
		"modern row with no parent at close never matches")

	// Qualified foreign filter: resolved to the spoke parent; the suffix
	// is cleared so it cannot leak onto local issues.
	foreign := auditParentFilter{
		resolvedUID: foreignParentUID,
		raw:         "spoke-project#abc4",
		has:         true,
	}
	assert.True(t, foreign.matches("abc4", &foreignUID),
		"foreign filter must match rows frozen under the foreign parent")
	assert.False(t, foreign.matches("abc4", &localUID),
		"foreign filter must not match rows under the same-suffix local parent")

	// Purged parent: nothing resolved, snapshot matching is the only
	// signal left and stays available for modern rows too.
	purged := auditParentFilter{
		parsedShortID: "abc4",
		raw:           "abc4",
		has:           true,
	}
	assert.True(t, purged.matches("abc4", &foreignUID),
		"purged-parent filters keep matching close-time snapshots")
}
