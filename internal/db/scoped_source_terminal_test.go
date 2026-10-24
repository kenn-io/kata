package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Decoder contracts only. These signatures are unit fixtures, not native root
// execution, cancellation, admission authority or stored receipt evidence.
func TestScopedSourceTerminalPublicResult(t *testing.T) {
	view, pins, keys := verificationView(t)
	legacy := SourceResult{OperationUID: verificationID(60), OperationDigest: strings.Repeat("a", 64), State: "canceled", ReasonCode: "conflict"}
	t.Run("legacy-exact-public-result", func(t *testing.T) {
		raw, err := json.Marshal(legacy)
		require.NoError(t, err)
		var expected SourceResult
		require.NoError(t, DecodeScopedRecord(raw, &expected))
		actual, err := DecodeScopedSourceTerminalResult(raw)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
		encoded, err := json.Marshal(actual)
		require.NoError(t, err)
		require.Equal(t, raw, encoded)
	})
	op := SourceOperation{Version: 3, UID: legacy.OperationUID, SourceInstanceUID: view.DestinationUID, CommandKind: "importIssues", Principal: CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: view.Route[0].BindingUID, Epoch: view.Route[0].Epoch, AccountableActor: "member", RouteDigest: view.RouteDigest}, ImportIntent: &ScopedImportIntent{ProjectUID: pins[0].ProjectUID, Source: "example-import", Items: []ScopedImportItem{}}}
	var err error
	op.Digest, err = SourceOperationDigest(op)
	require.NoError(t, err)
	// A canceled root intent has no native outputs or mapping effects. The
	// complete closed private seal still binds its random-salt-shaped bytes to
	// this exact terminal. Native generation of entropy/effects is tested later.
	execution := map[string]any{"version": 1, "project_uid": pins[0].ProjectUID, "salt": bytes.Repeat([]byte{7}, 32), "outputs": []any{}, "mapping_effects": []any{}}
	record := struct {
		OperationUID    string         `json:"operation_uid"`
		OperationDigest string         `json:"operation_digest"`
		State           string         `json:"state"`
		Execution       map[string]any `json:"execution"`
	}{op.UID, op.Digest, "canceled", execution}
	preimage, err := json.Marshal(struct {
		Domain string `json:"domain"`
		Record any    `json:"record"`
	}{"kata.scoped.import.execution.v1", record})
	require.NoError(t, err)
	canonical := jsontext.Value(preimage)
	require.NoError(t, canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)))
	result := SourceResult{OperationUID: op.UID, OperationDigest: op.Digest, State: "canceled", ReasonCode: "conflict", ImportOutcome: &ScopedImportOutcome{Commitment: ScopedOfferedDigest(canonical)}}
	signing, err := ScopedSourceResultSigningBytes(result)
	require.NoError(t, err)
	result.Signatures = verificationSign(signing, pins[:1], keys)
	rawResult, err := json.Marshal(result)
	require.NoError(t, err)
	var expected SourceResult
	require.NoError(t, DecodeScopedRecord(rawResult, &expected))
	t.Run("received-public-import-certificate", func(t *testing.T) {
		actual, err := DecodeScopedSourceTerminalResult(rawResult)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
		require.NoError(t, VerifyScopedSourceResult(actual, op, pins))
	})
	frame := func(t *testing.T) map[string]jsontext.Value {
		t.Helper()
		rawExecution, err := json.Marshal(execution)
		require.NoError(t, err)
		return map[string]jsontext.Value{"terminal_version": jsontext.Value(`1`), "result": bytes.Clone(rawResult), "execution": rawExecution}
	}
	t.Run("root-private-frame-returns-public-only", func(t *testing.T) {
		raw, err := json.Marshal(frame(t))
		require.NoError(t, err)
		actual, err := DecodeScopedSourceTerminalResult(raw)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
		require.NoError(t, VerifyScopedSourceResult(actual, op, pins))
		public, err := json.Marshal(actual)
		require.NoError(t, err)
		require.Equal(t, rawResult, public)
		for _, field := range []string{"execution", "terminal_version", "salt", "outputs", "mapping_effects"} {
			require.NotContains(t, string(public), `"`+field+`"`)
		}
	})
	t.Run("closed-and-commitment-bound", func(t *testing.T) {
		for _, mutate := range []func(map[string]jsontext.Value){
			func(f map[string]jsontext.Value) { f["terminal_version"] = jsontext.Value(`2`) },
			func(f map[string]jsontext.Value) { f["untyped_metadata"] = jsontext.Value(`{}`) },
			func(f map[string]jsontext.Value) { delete(f, "execution") },
			func(f map[string]jsontext.Value) { f["execution"] = jsontext.Value(`null`) },
			func(f map[string]jsontext.Value) { f["execution"] = jsontext.Value(`{}`) },
			func(f map[string]jsontext.Value) { f["result"] = jsontext.Value(`null`) },
			func(f map[string]jsontext.Value) { f["result"] = mustScopedTerminalJSON(t, legacy) },
			func(f map[string]jsontext.Value) {
				var fields map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(f["execution"], &fields))
				fields["salt"] = mustScopedTerminalJSON(t, bytes.Repeat([]byte{8}, 32))
				f["execution"] = mustScopedTerminalJSON(t, fields)
			},
			func(f map[string]jsontext.Value) {
				var fields map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(f["execution"], &fields))
				fields["outputs"] = jsontext.Value(`[{}]`)
				f["execution"] = mustScopedTerminalJSON(t, fields)
			},
		} {
			fields := frame(t)
			mutate(fields)
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			_, err = DecodeScopedSourceTerminalResult(raw)
			require.Error(t, err, "closed private metadata is neither ignored nor exposed")
		}
	})
}

func mustScopedTerminalJSON(t *testing.T, value any) jsontext.Value {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}
