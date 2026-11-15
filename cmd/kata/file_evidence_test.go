package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFileEvidence(t *testing.T) {
	for _, kind := range []string{"file", "file-sha256"} {
		got, err := parseEvidenceFlags([]string{kind + ":notes:reports/result.md"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "file", string(got[0].Type))
		require.Equal(t, "notes", got[0].File.Source)
		require.Equal(t, "reports/result.md", got[0].File.Path)
		if kind == "file-sha256" {
			require.NotNil(t, got[0].File.CaptureSHA256)
			require.True(t, *got[0].File.CaptureSHA256)
		}
	}
	for _, v := range []string{"file:", "file:notes:", "file:notes:../x", "file:/x", "file:notes:/x"} {
		_, err := parseEvidenceFlags([]string{v})
		require.Error(t, err, v)
	}
}
