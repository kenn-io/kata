//go:build rcpl_private

package sqlitestore

import (
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedProjectorCaptureIntegration(t *testing.T) {
	dbtest.RunScopedProjectorCaptureIntegration(t, func(t *testing.T) db.Storage {
		return openUIStore(t)
	})
}
