package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A V3 outcome certifies a root execution or terminal refusal without exposing
// native output cardinality, clocks, mapping observations or raw originals.
// These are codec/signature contracts, not native execution or custody tests.
func TestScopedImportResultCodec(t *testing.T) {
	view, pins, keys := verificationView(t)
	input := ScopedImportIntent{ProjectUID: pins[0].ProjectUID, Source: "example-import", Items: []ScopedImportItem{}}
	op := SourceOperation{Version: 3, UID: verificationID(50), SourceInstanceUID: view.DestinationUID, CommandKind: "importIssues", Principal: CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: view.Route[0].BindingUID, Epoch: view.Route[0].Epoch, AccountableActor: "member", RouteDigest: view.RouteDigest}, ImportIntent: &input}
	var err error
	op.Digest, err = SourceOperationDigest(op)
	require.NoError(t, err)
	decode := func(t *testing.T, state string, outcome jsontext.Value) (SourceResult, error) {
		t.Helper()
		fields := map[string]any{"operation_uid": op.UID, "operation_digest": op.Digest, "state": state, "reason_code": "", "members": []SourceMember{}, "receipts": []AttributionReceipt{}, "signatures": []ScopedSignature{}}
		if outcome != nil {
			fields["import_outcome"] = outcome
		}
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		var result SourceResult
		err = DecodeScopedRecord(raw, &result)
		return result, err
	}
	commitment := jsontext.Value(`{"commitment":"` + strings.Repeat("a", 64) + `"}`)
	t.Run("null-arm-preserves-old-decoder-refusal", func(t *testing.T) {
		// Before the new arm, strict source-result decoding refused this field.
		// Adding an optional pointer must not turn explicit null into absence.
		_, err := decode(t, "canceled", jsontext.Value(`null`))
		require.Error(t, err)
	})
	for _, state := range []string{"accepted", "rejected", "canceled"} {
		t.Run(state, func(t *testing.T) {
			result, err := decode(t, state, commitment)
			require.NoError(t, err, "the separate root outcome is legal without producer members or native outputs")
			require.NoError(t, result.Validate())
			raw, err := ScopedSourceResultSigningBytes(result)
			require.NoError(t, err)
			result.Signatures = verificationSign(raw, pins[:1], keys)
			require.NoError(t, VerifyScopedSourceResult(result, op, pins))
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			var fields map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(encoded, &fields))
			require.JSONEq(t, string(commitment), string(fields["import_outcome"]))
			for _, field := range []string{"outputs", "bindings", "salt", "events", "created", "updated", "unchanged", "mapping_rows"} {
				require.NotContains(t, fields, field)
			}
			changed := op
			changedInput := *op.ImportIntent
			changedInput.Source = "other-import"
			changed.ImportIntent = &changedInput
			changed.Digest, err = SourceOperationDigest(changed)
			require.NoError(t, err)
			require.Error(t, VerifyScopedSourceResult(result, changed, pins), "terminal is bound to the complete retained input")
			var outcome map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(fields["import_outcome"], &outcome))
			outcome["commitment"] = jsontext.Value(`"` + strings.Repeat("b", 64) + `"`)
			fields["import_outcome"], err = json.Marshal(outcome)
			require.NoError(t, err)
			tampered, err := json.Marshal(fields)
			require.NoError(t, err)
			var bad SourceResult
			require.NoError(t, DecodeScopedRecord(tampered, &bad))
			require.Error(t, VerifyScopedSourceResult(bad, op, pins), "opaque commitment is root signed")
			legacy := SourceOperation{Version: 1, UID: op.UID, SourceInstanceUID: op.SourceInstanceUID, CommandKind: "createIssue", Principal: op.Principal, Members: []SourceMember{{ProjectUID: input.ProjectUID, EventUID: verificationID(51), ContentHash: strings.Repeat("c", 64), Ordinal: 0}}}
			legacy.Digest, err = SourceOperationDigest(legacy)
			require.NoError(t, err)
			result.OperationDigest, result.Members = legacy.Digest, legacy.Members
			raw, err = ScopedSourceResultSigningBytes(result)
			require.NoError(t, err)
			result.Signatures = verificationSign(raw, pins[:1], keys)
			require.Error(t, VerifyScopedSourceResult(result, legacy, pins), "old operations cannot adopt the import outcome arm")
		})
	}
	t.Run("closed-arm", func(t *testing.T) {
		for _, value := range []string{`null`, `{}`, `{"commitment":"short"}`, `{"commitment":"` + strings.Repeat("a", 64) + `","outputs":[]}`, `{"commitment":"` + strings.Repeat("a", 64) + `","salt":"private"}`} {
			result, err := decode(t, "accepted", jsontext.Value(value))
			if err == nil {
				raw, err := ScopedSourceResultSigningBytes(result)
				require.NoError(t, err)
				result.Signatures = verificationSign(raw, pins[:1], keys)
				err = VerifyScopedSourceResult(result, op, pins)
			}
			require.Error(t, err, "invalid or private outcome data is refused: %s", value)
		}
		result, err := decode(t, "accepted", nil)
		require.NoError(t, err)
		raw, err := ScopedSourceResultSigningBytes(result)
		require.NoError(t, err)
		result.Signatures = verificationSign(raw, pins[:1], keys)
		require.Error(t, VerifyScopedSourceResult(result, op, pins), "V3 requires its separate signed root outcome")
	})
}
