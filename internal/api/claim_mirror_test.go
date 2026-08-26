package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

// TestMirrorDeprecatedClaimFieldsCopiesEveryPair is the enforcement point that
// replaces eight scattered mirroring assignments in the daemon handlers. The
// representation permits lease != claim in all seven pairs; nothing in the
// type system prevents it, so this test is what does. The zero and
// empty-slice cases matter because a nil slice and an empty slice are
// different JSON values and a mirror that reconstructed rather than copied
// would silently normalize one into the other.
func TestMirrorDeprecatedClaimFieldsCopiesEveryPair(t *testing.T) {
	lease := &api.IssueClaimOut{Holder: "alice", ClaimKind: "hard"}
	pending := []api.PendingClaimOut{{RequestUID: "req-1", Holder: "bob", ClaimKind: "timed"}}
	hubNow := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	violations := []api.ClaimViolationOut{{EventID: 7, EventUID: "evt-7", IssueUID: "iss-7"}}
	count := int64(1)

	showCases := []struct {
		name string
		body api.ShowIssueResponseBody
	}{
		{
			name: "populated",
			body: api.ShowIssueResponseBody{
				Lease:               lease,
				PendingLeases:       pending,
				LeaseHubNow:         &hubNow,
				LeaseViolations:     violations,
				LeaseViolationCount: &count,
			},
		},
		{name: "zero value", body: api.ShowIssueResponseBody{}},
		{
			name: "empty slices",
			body: api.ShowIssueResponseBody{
				PendingLeases:   []api.PendingClaimOut{},
				LeaseViolations: []api.ClaimViolationOut{},
			},
		},
	}
	for _, tc := range showCases {
		t.Run("ShowIssueResponseBody/"+tc.name, func(t *testing.T) {
			body := tc.body
			body.MirrorDeprecatedClaimFields()

			assert.Equal(t, body.Lease, body.Claim)
			assert.Equal(t, body.PendingLeases, body.PendingClaims)
			assert.Equal(t, body.LeaseHubNow, body.ClaimHubNow)
			assert.Equal(t, body.LeaseViolations, body.ClaimViolations)
			assert.Equal(t, body.LeaseViolationCount, body.ClaimViolationCount)
		})
	}

	for _, tc := range []struct {
		name string
		body api.ClaimActionResponseBody
	}{
		{name: "granted", body: api.ClaimActionResponseBody{Granted: true, Lease: lease}},
		{name: "denied", body: api.ClaimActionResponseBody{}},
	} {
		t.Run("ClaimActionResponseBody/"+tc.name, func(t *testing.T) {
			body := tc.body
			body.MirrorDeprecatedClaimFields()
			assert.Equal(t, body.Lease, body.Claim)
		})
	}

	for _, tc := range []struct {
		name string
		body api.ClaimStatusBody
	}{
		{name: "held", body: api.ClaimStatusBody{Held: true, Lease: lease, HubNow: hubNow}},
		{name: "free", body: api.ClaimStatusBody{HubNow: hubNow}},
	} {
		t.Run("ClaimStatusBody/"+tc.name, func(t *testing.T) {
			body := tc.body
			body.MirrorDeprecatedClaimFields()
			assert.Equal(t, body.Lease, body.Claim)
		})
	}
}

// TestMirroredBodiesPublishBothLeaseAndClaimKeys pins that claim* stays a
// deprecation and does not become an accidental removal. Both spellings are
// published in api/openapi.yaml and cannot be dropped without an
// APISchemaVersion bump.
func TestMirroredBodiesPublishBothLeaseAndClaimKeys(t *testing.T) {
	lease := &api.IssueClaimOut{Holder: "alice", ClaimKind: "hard"}
	hubNow := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	count := int64(1)

	show := api.ShowIssueResponseBody{
		Lease:               lease,
		PendingLeases:       []api.PendingClaimOut{{RequestUID: "req-1", Holder: "bob"}},
		LeaseHubNow:         &hubNow,
		LeaseViolations:     []api.ClaimViolationOut{{EventID: 7, EventUID: "evt-7"}},
		LeaseViolationCount: &count,
	}
	show.MirrorDeprecatedClaimFields()
	assertJSONHasKeys(t, show,
		"lease", "claim",
		"pending_leases", "pending_claims",
		"lease_hub_now", "claim_hub_now",
		"lease_violations", "claim_violations",
		"lease_violation_count", "claim_violation_count",
	)

	action := api.ClaimActionResponseBody{Granted: true, Lease: lease}
	action.MirrorDeprecatedClaimFields()
	assertJSONHasKeys(t, action, "lease", "claim")

	status := api.ClaimStatusBody{Held: true, Lease: lease, HubNow: hubNow}
	status.MirrorDeprecatedClaimFields()
	assertJSONHasKeys(t, status, "lease", "claim")
}

func assertJSONHasKeys(t *testing.T, value any, keys ...string) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &decoded))
	for _, key := range keys {
		require.Contains(t, decoded, key, "response must still carry %q", key)
	}
}
