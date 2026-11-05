package pgstore

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronValidateSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	reader, err := OpenWithConfig(t.Context(), dsn, Config{Schema: "cron_validate_snapshot", SchemaMode: SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	dbtest.CheckCronValidateSnapshot(t, reader.cronSQL(), reader)
}
