package db

import (
	legacyjson "encoding/json"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedAuthorityParamsAreNativeOnly(t *testing.T) {
	params := CreateScopedRelayEnrollmentParams{CatalogEntryUID: "00000000000000000000000211", AnchorProjectID: 41, ParentTokenID: 42, Token: "example-transport-token"}
	_, err := json.Marshal(params)
	require.Error(t, err)
	_, err = legacyjson.Marshal(params)
	require.Error(t, err)
	for _, raw := range []string{`{}`, `null`, `{"ParentTokenID":99,"AnchorProjectID":100}`, `{"parent_token_id":99,"anchor_project_id":100}`} {
		got := params
		require.Error(t, json.Unmarshal([]byte(raw), &got))
		require.Equal(t, params, got, "wire rejection must not replace native admission")
		require.Error(t, legacyjson.Unmarshal([]byte(raw), &got))
		require.Equal(t, params, got)
	}
}
