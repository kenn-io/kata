package sqlitestore_test

import (
	"testing"

	"go.kenn.io/kata/internal/db/dbtest"
)

func TestFederationSpokeNames(t *testing.T) { dbtest.RunFederationSpokeNames(t, openTestDB(t)) }
