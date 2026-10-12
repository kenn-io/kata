package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestGenerateOmitsUnusedStandardLibraryImports(t *testing.T) {
	storagePath := filepath.Join(t.TempDir(), "storage.go")
	require.NoError(t, os.WriteFile(storagePath, []byte(`package db
import "context"
type Storage interface { Only(context.Context) error }
`), 0o600))

	generated, err := Generate(storagePath)
	require.NoError(t, err)
	file, err := parser.ParseFile(token.NewFileSet(), "stubs_gen.go", generated, parser.ImportsOnly)
	require.NoError(t, err)
	var imports []string
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err)
		imports = append(imports, path)
	}
	assert.Equal(t, []string{"context", "go.kenn.io/kata/internal/db"}, imports)
}

func TestStorageMethodInventoryClassifiesEveryMethod(t *testing.T) {
	methods, err := CollectStorageMethodInventory("../../storage.go")
	require.NoError(t, err)

	var stubbed []string
	for _, method := range methods {
		switch method.Status {
		case MethodImplemented:
		case MethodStubbed:
			stubbed = append(stubbed, method.Name)
		default:
			assert.Fail(t, "unknown method status", "%s has status %q", method.Name, method.Status)
		}
	}

	assert.Empty(t, stubbed)
}

func TestImplementedStorageMethodsHaveConformanceCoverage(t *testing.T) {
	methods, err := CollectStorageMethodInventory("../../storage.go")
	require.NoError(t, err)

	covered := dbtest.CoveredStorageMethods(pgstore.ExpectedConformanceFailures())
	declared := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		declared[method.Name] = struct{}{}
		if method.Status == MethodImplemented {
			assert.Contains(t, covered, method.Name,
				"implemented method %s needs a shared conformance scenario", method.Name)
		}
	}
	for method := range covered {
		assert.Contains(t, declared, method,
			"conformance coverage names unknown Storage method %s", method)
	}
}

// TestGeneratedStubsMatchSource regenerates stubs_gen.go in memory and
// compares against the committed file. A drift means someone touched
// internal/db/storage.go (added a method, changed a signature) without
// re-running `go generate`, or hand-edited stubs_gen.go directly.
//
// The committed stubs_gen.go is the source of truth at build time; this
// test guards against a stale-but-still-compiling state where the
// interface has grown a method we never stubbed.
func TestGeneratedStubsMatchSource(t *testing.T) {
	got, err := Generate("../../storage.go")
	require.NoError(t, err)

	want, err := os.ReadFile("../stubs_gen.go")
	require.NoError(t, err)

	assert.Equal(t, string(want), string(got),
		"stubs_gen.go is out of date — run `go generate ./internal/db/pgstore`")
}
