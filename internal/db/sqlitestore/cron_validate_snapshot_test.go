package sqlitestore

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestCronValidateSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	reader, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	writer, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	dbtest.CheckCronValidateSnapshot(t, reader.cronSQL(), writer)
}
