package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCronValidateParentDocumentStage(t *testing.T) {
	f := newCronFixture(t)
	parent, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Periodic review", Author: "operator"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		issue any
		omit  bool
	}{
		{"missing", nil, true}, {"null", nil, false}, {"array", []any{}, false}, {"string", "invalid", false}, {"number", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := cronDefinition()
			delete(definition, "issue")
			if !tc.omit {
				definition["issue"] = tc.issue
			}
			result := cronOK(t, f.session, "cron_job_validate", map[string]any{"name": "Inspect", "definition": definition, "parent_ref": parent.ShortID})
			require.Equal(t, false, result["valid"])
			failures := result["errors"].([]any)
			require.Len(t, failures, 1)
			require.Equal(t, "document", failures[0].(map[string]any)["stage"])
		})
	}
	// Preserve the reviewer's exact empty-definition reproduction too.
	result := cronOK(t, f.session, "cron_job_validate", map[string]any{"name": "Inspect", "definition": map[string]any{}, "parent_ref": parent.ShortID})
	require.Equal(t, false, result["valid"])
	require.Equal(t, "document", result["errors"].([]any)[0].(map[string]any)["stage"])
}
