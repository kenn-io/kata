//go:build rcpl_private

package pgstore

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedProjectorCaptureIntegration(t *testing.T) {
	require.NotEmpty(t, os.Getenv("KATA_TEST_POSTGRES_DSN"), "native join requires the runner's disposable PostgreSQL DSN; a skip is not proof")
	dbtest.RunScopedProjectorCaptureIntegration(t, func(t *testing.T) db.Storage {
		return openUIStore(t)
	})
}
