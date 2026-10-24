package daemon

import (
	"testing"

	"go.kenn.io/kata/internal/db"
)

func TestScopedContractInventoryComplete(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	for id := range registeredOperations(srv.API().OpenAPI()) {
		class := db.SourceCommandClass(id)
		if class.Disposition == "" || class.Gate == "" {
			t.Errorf("unclassified operation %s: %+v", id, class)
		}
	}
}
