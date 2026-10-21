package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIssueKeysAreRegistered(t *testing.T) {
	for _, key := range []string{"scheduled_on", "deadline_on", "someday", "checklist", "timezone"} {
		kind, ok := IssueRegistry[key]
		assert.True(t, ok, "issue key %s missing from registry", key)
		assert.NotZero(t, kind.Type, "issue key %s has zero-value Type", key)
	}
}

func TestProjectKeysAreRegistered(t *testing.T) {
	for _, key := range []string{"area"} {
		kind, ok := ProjectRegistry[key]
		assert.True(t, ok, "project key %s missing from registry", key)
		assert.NotZero(t, kind.Type, "project key %s has zero-value Type", key)
	}
}

// TestRegistriesAreNarrow guards against silently expanding the reserved-key
// set. Reserved keys carry semantic load on the daemon side; everything else
// must be accepted opaquely. If you intentionally reserve a new key, update
// this test in the same commit so the trade-off is reviewed.
func TestRegistriesAreNarrow(t *testing.T) {
	assert.Len(t, IssueRegistry, 5,
		"IssueRegistry should hold exactly the 5 reserved keys")
	assert.Len(t, ProjectRegistry, 1,
		"ProjectRegistry should hold exactly the 1 reserved key")
}

func TestUnknownKeyNotPresent(t *testing.T) {
	_, ok := IssueRegistry["definitely_not_a_key"]
	assert.False(t, ok)
	_, ok = ProjectRegistry["definitely_not_a_key"]
	assert.False(t, ok)
}
