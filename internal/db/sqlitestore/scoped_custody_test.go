package sqlitestore

import (
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedCustodyWholeManifest(t *testing.T) {
	dbtest.RunScopedCustodyWholeManifest(t, openUIStore(t))
}

func TestScopedCustodyLiveAuthority(t *testing.T) {
	dbtest.RunScopedCustodyLiveAuthority(t, openUIStore(t))
}

func TestScopedCustodyExactBytes(t *testing.T) {
	dbtest.RunScopedCustodyExactBytes(t, openUIStore(t))
}

func TestScopedCustodyOpaqueExpectationForwarding(t *testing.T) {
	dbtest.RunScopedCustodyOpaqueExpectationForwarding(t, openUIStore(t))
}

func TestScopedCustodyOpaqueTokenOwnerRestore(t *testing.T) {
	dbtest.RunScopedCustodyOpaqueTokenOwnerRestore(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedCustodyOpaqueIntermediateRestore(t *testing.T) {
	dbtest.RunScopedCustodyOpaqueIntermediateRestore(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedExpectationStablePreparation(t *testing.T) {
	dbtest.RunScopedExpectationStablePreparation(t, openUIStore(t))
}

func TestScopedOpaqueGuardABA(t *testing.T) {
	dbtest.RunScopedOpaqueGuardABA(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}

func TestScopedOpaqueGuardIncarnationCap(t *testing.T) {
	dbtest.RunScopedOpaqueGuardIncarnationCap(t, func(t *testing.T) db.Storage { return openUIStore(t) })
}
