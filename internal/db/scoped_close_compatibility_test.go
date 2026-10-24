package db

import (
	"bytes"
	"encoding/json/jsontext"
	"testing"
)

func TestScopedCloseLegacyCarrierAbsent(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"numeric uid", `{"label":"ready","uid":7}`},
		{"numeric status", `{"label":"ready","status":9}`},
		{"object reason", `{"label":"ready","reason":{"detail":"legacy-extension"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := jsontext.Value(tc.raw)
			before := bytes.Clone(raw)
			if err := ValidateFederationEntries("issue.labeled", closeProofID(50), raw); err != nil {
				t.Fatal("carrier-absent legacy event refused:", err)
			}
			if !bytes.Equal(before, raw) {
				t.Fatal("changed retained native payload")
			}
		})
	}
}

func TestScopedCloseCarrierPresentRemainsStrict(t *testing.T) {
	for _, raw := range []string{
		`{"label":"ready","uid":7,"close_value":null}`,
		`{"label":"ready","status":9,"close_state":null}`,
		`{"uid":7,"reason":"duplicate","close_value":{"kind":"duplicate","peer_project_uid":"00000000000000000000000003","peer_issue_uid":"00000000000000000000000004"}}`,
		`{"uid":"00000000000000000000000001","reason":"duplicate","close_value":{"kind":"duplicate","peer_project_uid":"00000000000000000000000003","peer_issue_uid":"00000000000000000000000004","extra":1}}`,
	} {
		before := []byte(raw)
		if err := ValidateFederationEntries("issue.closed", closeProofID(50), before); err == nil {
			t.Fatalf("malformed present close carrier accepted: %s", raw)
		}
		if !bytes.Equal(before, []byte(raw)) {
			t.Fatal("changed rejected native payload")
		}
	}
}

// Both native legacy stores retain these known metadata events as no-ops.
// JSON values without top-level object fields cannot carry reserved close state.
func TestScopedCloseLegacyMetadataCarrierAbsent(t *testing.T) {
	for _, raw := range []string{` 9 `, ` [ 7, {"status":9} ] `} {
		before := []byte(raw)
		if err := ValidateFederationEntries("project.metadata_updated", closeProofID(50), before); err != nil {
			t.Errorf("native-valid metadata JSON value refused: %s: %v", raw, err)
		}
		if !bytes.Equal(before, []byte(raw)) {
			t.Fatal("changed retained native metadata payload")
		}
	}
}
