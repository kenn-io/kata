package pgstore

import (
	"testing"

	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedDefinitionPreparation(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			dbtest.RunScopedDefinitionPreparation(t, openUIStore(t), kind)
		})
	}
}
