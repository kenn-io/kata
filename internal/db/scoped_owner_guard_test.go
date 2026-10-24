package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The ratified V2 owner guard commits a literal nullable-owner comparison. It
// cannot change V1's transcript or be smuggled through a null/unknown field.
func TestScopedOwnerGuardCodec(t *testing.T) {
	principal := CustodyPrincipal{AuthorityUID: verificationID(9), BindingUID: verificationID(7), Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64)}
	op := SourceOperation{Version: 1, UID: verificationID(50), Digest: strings.Repeat("d", 64), SourceInstanceUID: verificationID(8), CommandKind: "unassignIssue", Principal: principal, Members: []SourceMember{{ProjectUID: verificationID(1), EventUID: verificationID(51), ContentHash: strings.Repeat("c", 64), Ordinal: 0}}, Membership: []ScopedMembershipProof{{Endpoint: ScopedEndpoint{ProjectUID: verificationID(1), IssueUID: verificationID(40)}, Kind: "root_membership"}}}
	v1, err := json.Marshal(op)
	require.NoError(t, err)
	wantV1 := `{"version":1,"uid":"00000000000000000000000050","digest":"` + strings.Repeat("d", 64) + `","source_instance_uid":"00000000000000000000000008","command_kind":"unassignIssue","idempotency_key":"","principal":{"epoch":1,"binding_uid":"00000000000000000000000007","authority_uid":"00000000000000000000000009","accountable_actor":"member","route_digest":"` + strings.Repeat("a", 64) + `"},"members":[{"project_uid":"00000000000000000000000001","event_uid":"00000000000000000000000051","content_hash":"` + strings.Repeat("c", 64) + `","ordinal":0}],"membership":[{"endpoint":{"project_uid":"00000000000000000000000001","issue_uid":"00000000000000000000000040"},"kind":"root_membership","creating_operation_uid":"","signatures":[]}],"expectations":[]}`
	require.Equal(t, wantV1, string(v1), "VERSION1 bytes remain exact")
	guard := `{"subject":{"project_uid":"00000000000000000000000001","issue_uid":"00000000000000000000000040"},"expected_owner":"member"}`
	wire := func(version int, kind string, ownerGuard string, present bool) []byte {
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(v1, &fields))
		fields["version"], err = json.Marshal(version)
		require.NoError(t, err)
		fields["command_kind"], err = json.Marshal(kind)
		require.NoError(t, err)
		if present {
			fields["owner_guard"] = jsontext.Value(ownerGuard)
		}
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		return raw
	}
	for _, owner := range []string{"member", "", " member "} {
		t.Run("literal-"+owner, func(t *testing.T) {
			ownerBytes, err := json.Marshal(owner)
			require.NoError(t, err)
			literal := bytes.Replace([]byte(guard), []byte(`"member"`), ownerBytes, 1)
			var decoded SourceOperation
			require.NoError(t, DecodeScopedRecord(wire(2, "unassignIssue", string(literal), true), &decoded))
			encoded, err := json.Marshal(decoded)
			require.NoError(t, err)
			var fields map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(encoded, &fields))
			var retained struct {
				ExpectedOwner string `json:"expected_owner"`
			}
			require.NoError(t, json.Unmarshal(fields["owner_guard"], &retained))
			require.Equal(t, owner, retained.ExpectedOwner, "no normalization or nil/empty collapse")
			digest, err := SourceOperationDigest(decoded)
			require.NoError(t, err)
			other := wire(2, "unassignIssue", strings.Replace(string(literal), string(ownerBytes), `"different-owner"`, 1), true)
			var changed SourceOperation
			require.NoError(t, DecodeScopedRecord(other, &changed))
			changedDigest, err := SourceOperationDigest(changed)
			require.NoError(t, err)
			require.NotEqual(t, digest, changedDigest, "changed guard must conflict with exact retry")
		})
	}
	for _, tc := range []struct {
		name, kind, guard string
		version           int
		present           bool
	}{
		{"v1-null", "unassignIssue", "null", 1, true},
		{"v1-object", "unassignIssue", guard, 1, true},
		{"v2-missing", "unassignIssue", "", 2, false},
		{"v2-null", "unassignIssue", "null", 2, true},
		{"unsupported-kind", "editIssue", guard, 2, true},
		{"unsupported-version", "unassignIssue", guard, 3, true},
		{"missing-owner", "unassignIssue", `{"subject":{"project_uid":"00000000000000000000000001","issue_uid":"00000000000000000000000040"}}`, 2, true},
		{"null-owner", "unassignIssue", strings.Replace(guard, `"member"`, "null", 1), 2, true},
		{"nonstring-owner", "unassignIssue", strings.Replace(guard, `"member"`, "42", 1), 2, true},
		{"missing-subject", "unassignIssue", `{"expected_owner":"member"}`, 2, true},
		{"unknown-field", "unassignIssue", strings.TrimSuffix(guard, "}") + `,"incarnation":1}`, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decoded SourceOperation
			require.Error(t, DecodeScopedRecord(wire(tc.version, tc.kind, tc.guard, tc.present), &decoded))
		})
	}
}
