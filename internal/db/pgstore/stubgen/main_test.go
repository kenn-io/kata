package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	require.NotEmpty(t, methods)
	for _, method := range methods {
		assert.Equal(t, MethodImplemented, method.Status,
			"pgstore must implement Storage method %s instead of serving a generated stub", method.Name)
	}
}

func TestStorageMethodInventoryMarksUnportedMethodsStubbed(t *testing.T) {
	names := append(implementedMethodNames(), "UnportedMethod")
	inventory, err := CollectStorageMethodInventory(writeStorageFixture(t, names))
	require.NoError(t, err)
	statuses := make(map[string]MethodStatus, len(inventory))
	for _, method := range inventory {
		statuses[method.Name] = method.Status
	}
	assert.Equal(t, MethodStubbed, statuses["UnportedMethod"])
	assert.Equal(t, MethodImplemented, statuses["APITokenByID"])
}

func TestStorageMethodInventoryRejectsStaleImplementedEntry(t *testing.T) {
	names := slices.DeleteFunc(implementedMethodNames(), func(name string) bool { return name == "APITokenByID" })
	_, err := CollectStorageMethodInventory(writeStorageFixture(t, names))
	require.EqualError(t, err, `implemented method "APITokenByID" is not declared by Storage`)
}

func implementedMethodNames() []string {
	names := make([]string, 0, len(alreadyImplemented))
	for name := range alreadyImplemented {
		names = append(names, name)
	}
	return names
}

// writeStorageFixture writes a Storage interface that declares names.
func writeStorageFixture(t *testing.T, names []string) string {
	t.Helper()
	var source strings.Builder
	source.WriteString("package db\ntype Storage interface {\n")
	for _, name := range names {
		source.WriteString("\t" + name + "()\n")
	}
	source.WriteString("}\n")
	storagePath := filepath.Join(t.TempDir(), "storage.go")
	require.NoError(t, os.WriteFile(storagePath, []byte(source.String()), 0o600))
	return storagePath
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
