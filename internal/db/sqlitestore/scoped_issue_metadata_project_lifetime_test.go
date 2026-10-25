package sqlitestore

import (
	"testing"

	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedIssueMetadataProjectLifetime(t *testing.T) {
	dbtest.RunScopedIssueMetadataProjectLifetime(t, openUIStore(t))
}
