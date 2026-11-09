package jsonl_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func TestFederationSpokeNamesBackup(t *testing.T) {
	source := openExportTestDB(t)
	const spoke = "01HZNQ7VFPK1XGD8R5MABCD4EA"
	_, err := source.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke, SpokeName: "laptop", Capabilities: "pull", Actor: "member"})
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	for _, standalone := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "standalone"}[standalone], func(t *testing.T) {
			target := openImportTargetDB(t)
			require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(backup.Bytes()), target, jsonl.ImportOptions{AsStandalone: standalone}))
			names, err := target.ListFederationSpokeNames(t.Context())
			require.NoError(t, err)
			if standalone {
				require.Empty(t, names)
			} else {
				require.Equal(t, "laptop", names[spoke])
			}
		})
	}
}
