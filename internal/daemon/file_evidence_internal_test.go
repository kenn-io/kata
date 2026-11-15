package daemon

import (
	"path/filepath"
	"testing"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db/sqlitestore"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/fileevidence"
)

func TestFileEvidenceStructuralCheckSurvivesTUIBypass(t *testing.T) {
	err := captureFileEvidence(t.Context(), ServerConfig{}, []db.Evidence{{Type: "external", Account: "reviewed", File: &fileevidence.Reference{Source: "notes", Path: "x"}}})
	require.Error(t, err)
}

func TestOverruleHintPreservesAttachmentEvidence(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "fixture.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Attachment proof", Author: "worker"})
	require.NoError(t, err)
	body := api.CloseActionRequestBody{Reason: "done", Message: "Completed the requested change and retained its attachment proof.", Evidence: []api.Evidence{{Type: api.EvidenceAttachment, AttachmentID: "report-id"}}}
	refusal := api.NewError(409, "open_refutations", "open refutations", "", map[string]any{"refutations": []db.OpenRefutation{{Refutation: "c:example"}}})
	got := closeRefusalHint(t.Context(), store, issue, body, refusal)
	var apiErr *api.APIError
	require.ErrorAs(t, got, &apiErr)
	require.Contains(t, apiErr.Hint, "--evidence 'attachment:report-id'")
}
