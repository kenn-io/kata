package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// PostgreSQL CI selects these tests with -run Postgres, even when local runs skip them.
func TestPostgresTestsAreSelectedByName(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || (fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") && strings.Contains(fn.Name.Name, "Postgres")) {
				continue
			}
			for node := range ast.Preorder(fn.Body) {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					continue
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				switch selector.Sel.Name {
				case "NewPostgresContainer", "NewPostgresWithPgvectorContainer", "NewPlainPostgresContainer":
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "testenv" {
						t.Errorf("%s: %s calls testenv.%s but is not a top-level Test with Postgres in its name", path, fn.Name.Name, selector.Sel.Name)
					}
				}
			}
		}
	}
}
