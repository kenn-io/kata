package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/fileevidence"
)

func TestFileEvidenceStructuralCheckSurvivesTUIBypass(t *testing.T) {
	err := captureFileEvidence(t.Context(), ServerConfig{}, []db.Evidence{{Type: "external", Account: "reviewed", File: &fileevidence.Reference{Source: "notes", Path: "x"}}})
	require.Error(t, err)
}
