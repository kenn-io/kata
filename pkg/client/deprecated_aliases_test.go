package client

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client/generated"
)

// ReadyGlobalIssue was renamed to ReadyGlobalIssueOut when ready rows were
// hydrated to full IssueOut. The deprecated alias must keep pre-rename
// consumers compiling and decoding the expanded payload, labels included.
func TestDeprecatedReadyGlobalIssueAliasDecodesHydratedRow(t *testing.T) {
	rowJSON := []byte(`{
		"uid":"01TESTISSUEAAAAAAAAAAAAAAA",
		"short_id":"abc4",
		"qualified_id":"spoke-project#abc4",
		"title":"ready row",
		"body":"ready body",
		"status":"open",
		"author":"tester",
		"created_at":"2026-06-23T00:00:00Z",
		"updated_at":"2026-06-23T00:00:00Z",
		"project_id":1,
		"project_name":"spoke-project",
		"labels":["infra","p1"]
	}`)
	var issue generated.ReadyGlobalIssue //nolint:staticcheck // exercising the deprecated alias is the point
	require.NoError(t, json.Unmarshal(rowJSON, &issue))
	require.NoError(t, issue.Validate())
	require.Equal(t, []string{"infra", "p1"}, issue.Labels)
	require.Equal(t, "spoke-project", issue.ProjectName)
}
