package client

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client/generated"
)

// A changes block whose edit touched no parent carries no parent_set or
// parent_removed peer; the generated client must validate and round-trip it
// without inventing empty peer objects.
func TestLinkChangesWithoutParentPeersValidatesAndOmitsParentPeers(t *testing.T) {
	changes := generated.LinkChanges{}
	require.NoError(t, changes.Validate(),
		"a changes block without parent peers must validate")

	out, err := json.Marshal(changes)
	require.NoError(t, err)
	var roundTrip map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &roundTrip))
	require.NotContains(t, roundTrip, "parent_set",
		"absent parent_set must not marshal as an empty object")
	require.NotContains(t, roundTrip, "parent_removed",
		"absent parent_removed must not marshal as an empty object")
}

// A parentless issue response carries no parent peer. The generated client
// must keep IssueOut.Parent a pointer so a parentless IssueOut validates and
// round-trips without inventing an empty parent object — the regression that
// surfaces when an optional object response field regenerates as a value
// type (additionalProperties leaking onto the client-flavor schema).
func TestIssueOutWithoutParentValidatesAndOmitsParent(t *testing.T) {
	issue := generated.IssueOut{
		UID:         "01TESTISSUEAAAAAAAAAAAAAAA",
		ShortID:     "abc4",
		QualifiedID: "spoke-project#abc4",
		Title:       "parentless issue",
		Body:        "no parent here",
		Status:      "open",
		Author:      "tester",
		CreatedAt:   time.Unix(1, 0).UTC(),
		UpdatedAt:   time.Unix(1, 0).UTC(),
	}
	require.NoError(t, issue.Validate(),
		"a parentless issue must validate without a parent peer")

	out, err := json.Marshal(issue)
	require.NoError(t, err)
	var roundTrip map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &roundTrip))
	require.NotContains(t, roundTrip, "parent",
		"absent parent must not marshal as an empty object")
}
