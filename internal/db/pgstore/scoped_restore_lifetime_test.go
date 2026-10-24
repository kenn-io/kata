package pgstore

import (
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedOwnerBackupRequiresRootWork(t *testing.T) {
	dbtest.RunScopedOwnerBackupRequiresRootWork(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedRetainedProjectLifetime(t *testing.T) {
	dbtest.RunScopedRetainedProjectLifetime(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedCloseSubjectFirstOwnerBackup(t *testing.T) {
	dbtest.RunScopedCloseSubjectFirstOwnerBackup(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedCloseUnboundProjectLifetime(t *testing.T) {
	dbtest.RunScopedCloseUnboundProjectLifetime(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}
