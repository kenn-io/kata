package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The ratified import intent is an exclusive V3 arm. It retains normalized
// input, not fabricated producer events or private root mapping decisions.
func TestScopedImportIntentCodec(t *testing.T) {
	principal := CustodyPrincipal{AuthorityUID: verificationID(9), BindingUID: verificationID(7), Epoch: 1, AccountableActor: "member", RouteDigest: strings.Repeat("a", 64)}
	legacy := SourceOperation{Version: 1, UID: verificationID(50), Digest: strings.Repeat("d", 64), SourceInstanceUID: verificationID(8), CommandKind: "importIssues", Principal: principal, Members: []SourceMember{{ProjectUID: verificationID(1), EventUID: verificationID(51), ContentHash: strings.Repeat("c", 64), Ordinal: 0}}}
	legacyRaw, err := json.Marshal(legacy)
	require.NoError(t, err)
	intent := jsontext.Value(`{"project_uid":"00000000000000000000000001","source":"example-import","items":[{"external_id":"first","legacy_external_ids":["legacy-first","older-first"],"title":"First","body":"body","author":"import-author","owner":"","priority":0,"status":"open","closed_reason":null,"created_at":"2026-10-01T12:00:00Z","updated_at":"2026-10-02T12:00:00Z","closed_at":null,"labels":["one","two"],"comments":[{"external_id":"comment-first","legacy_external_ids":["comment-legacy"],"author":"comment-author","body":"note","created_at":"2026-10-01T12:01:00Z"}],"links":[{"type":"related","target_external_id":"second"}],"link_types_authoritative":null},{"external_id":"second","legacy_external_ids":[],"title":"Second","body":"","author":"import-author","owner":null,"priority":null,"status":"open","closed_reason":null,"created_at":"2026-10-01T12:00:00Z","updated_at":"2026-10-01T12:00:00Z","closed_at":null,"labels":[],"comments":[],"links":[],"link_types_authoritative":{}}]}`)
	operation := func(t *testing.T) map[string]jsontext.Value {
		t.Helper()
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(legacyRaw, &fields))
		fields["version"] = jsontext.Value(`3`)
		fields["import_intent"] = bytes.Clone(intent)
		for _, name := range []string{"members", "membership", "expectations", "owner_guard"} {
			delete(fields, name)
		}
		return fields
	}
	decode := func(t *testing.T, fields map[string]jsontext.Value) (SourceOperation, error) {
		t.Helper()
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		var op SourceOperation
		err = json.Unmarshal(raw, &op)
		return op, err
	}
	t.Run("complete-eventless-roundtrip", func(t *testing.T) {
		op, err := decode(t, operation(t))
		require.NoError(t, err)
		op.Digest, err = SourceOperationDigest(op)
		require.NoError(t, err)
		input := SourceSubmission{Operation: op}
		require.NoError(t, input.Validate())
		wire, err := EncodeScopedSourceSubmission(input)
		require.NoError(t, err)
		// Exact authenticated carrier bytes remain the retry identity even when
		// no producer originals exist and root-native outputs are not known yet.
		spaced := append([]byte(" \n"), append(wire, []byte("\n ")...)...)
		retained, err := DecodeScopedSourceSubmission(spaced)
		require.NoError(t, err)
		require.Empty(t, retained.Originals)
		require.Empty(t, retained.Operation.Members)
		require.Empty(t, retained.Operation.Membership)
		require.Empty(t, retained.Operation.Expectations)
		again, err := EncodeScopedSourceSubmission(retained)
		require.NoError(t, err)
		require.Equal(t, spaced, again)
		opRaw, err := json.Marshal(op)
		require.NoError(t, err)
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(opRaw, &fields))
		for _, name := range []string{"members", "membership", "expectations", "owner_guard"} {
			require.NotContains(t, fields, name, "V3 cannot present a producer-event/guard arm")
		}
		var body struct {
			Items []map[string]jsontext.Value `json:"items"`
		}
		require.NoError(t, json.Unmarshal(fields["import_intent"], &body))
		require.JSONEq(t, `["legacy-first","older-first"]`, string(body.Items[0]["legacy_external_ids"]))
		require.JSONEq(t, `null`, string(body.Items[0]["link_types_authoritative"]))
		require.JSONEq(t, `{}`, string(body.Items[1]["link_types_authoritative"]))
	})
	t.Run("exclusive-versioned-arm", func(t *testing.T) {
		for _, extra := range []struct{ name, value string }{
			{"members", `[]`}, {"membership", `[]`}, {"expectations", `[]`}, {"owner_guard", `null`},
		} {
			fields := operation(t)
			fields[extra.name] = jsontext.Value(extra.value)
			_, err := decode(t, fields)
			require.Error(t, err, "V3 rejects presence of %s even empty/null", extra.name)
		}
		for _, value := range []string{"null", "{}"} {
			fields := operation(t)
			fields["import_intent"] = jsontext.Value(value)
			_, err := decode(t, fields)
			require.Error(t, err)
		}
		fields := operation(t)
		delete(fields, "import_intent")
		_, err := decode(t, fields)
		require.Error(t, err)
		fields = operation(t)
		fields["command_kind"] = jsontext.Value(`"createIssue"`)
		_, err = decode(t, fields)
		require.Error(t, err)
		guarded := legacy
		guarded.Version, guarded.CommandKind = 2, "unassignIssue"
		endpoint := ScopedEndpoint{ProjectUID: verificationID(1), IssueUID: verificationID(40)}
		guarded.Membership = []ScopedMembershipProof{{Kind: "root_membership", Endpoint: endpoint}}
		guarded.OwnerGuard = &SourceOwnerGuard{Subject: endpoint, ExpectedOwner: "member"}
		guardedRaw, err := json.Marshal(guarded)
		require.NoError(t, err)
		for _, version := range []string{"1", "2"} {
			for _, value := range []jsontext.Value{jsontext.Value(`null`), intent} {
				base := legacyRaw
				if version == "2" {
					base = guardedRaw
				}
				var fields map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(base, &fields))
				fields["version"], fields["import_intent"] = jsontext.Value(version), value
				_, err := decode(t, fields)
				require.Error(t, err, "old versions reject import_intent presence")
			}
		}
		var old SourceOperation
		require.NoError(t, json.Unmarshal(legacyRaw, &old))
		oldRaw, err := json.Marshal(old)
		require.NoError(t, err)
		require.Equal(t, legacyRaw, oldRaw, "unchanged V1 transcript")
	})
	t.Run("no-internal-or-local-authority-fields", func(t *testing.T) {
		for _, extra := range []struct{ name, value string }{
			{"issue_sync_guard", `null`}, {"manage_status_separately", `false`},
			{"reconcile_link_types_for_unchanged", `{}`}, {"reconcile_labels_for_unchanged", `{}`},
			{"reconcile_status_for_unchanged", `false`}, {"import_status_observations", `{}`},
			{"preserve_local_parent_conflicts", `false`}, {"project_id", `1`}, {"actor", `"other"`},
			{"birth_uid", `"00000000000000000000000070"`}, {"target_uid", `"00000000000000000000000071"`},
		} {
			fields := operation(t)
			var input map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(fields["import_intent"], &input))
			input[extra.name] = jsontext.Value(extra.value)
			fields["import_intent"], err = json.Marshal(input)
			require.NoError(t, err)
			_, err := decode(t, fields)
			require.Error(t, err, "forbidden intent field %s is rejected, not dropped", extra.name)
		}
	})
	t.Run("complete-input-is-digest-identity", func(t *testing.T) {
		fields := operation(t)
		op, err := decode(t, fields)
		require.NoError(t, err)
		before, err := SourceOperationDigest(op)
		require.NoError(t, err)
		var input map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(intent, &input))
		var items []map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(input["items"], &items))
		items[0]["legacy_external_ids"] = jsontext.Value(`["older-first","legacy-first"]`)
		input["items"], err = json.Marshal(items)
		require.NoError(t, err)
		fields["import_intent"], err = json.Marshal(input)
		require.NoError(t, err)
		changed, err := decode(t, fields)
		require.NoError(t, err)
		after, err := SourceOperationDigest(changed)
		require.NoError(t, err)
		require.NotEqual(t, before, after, "ordered legacy adoption is not a set")
		items[0]["legacy_external_ids"] = jsontext.Value(`["legacy-first","older-first"]`)
		items[0]["link_types_authoritative"] = jsontext.Value(`{}`)
		input["items"], err = json.Marshal(items)
		require.NoError(t, err)
		fields["import_intent"], err = json.Marshal(input)
		require.NoError(t, err)
		changed, err = decode(t, fields)
		require.NoError(t, err)
		after, err = SourceOperationDigest(changed)
		require.NoError(t, err)
		require.NotEqual(t, before, after, "nil vs empty authority has native meaning")
	})
}
