// Package main checks literal testify polling budgets in Go tests.
package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	assertImport  = "github.com/stretchr/testify/assert"
	requireImport = "github.com/stretchr/testify/require"
)

type budgetKey struct {
	Path      string
	Function  string
	Assertion string
	Duration  time.Duration
}

type budgetOccurrence struct {
	Key  budgetKey
	Line int
}

type diagnostic struct {
	Path string
	Line int
	Text string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, defaultAllowances()))
}

func defaultAllowances() map[budgetKey]int {
	return map[budgetKey]int{
		{
			Path:      "internal/federation/config_provider_test.go",
			Function:  "TestReconcilerStopsTerminalProviderDecisionsWithoutDroppingCleanup",
			Assertion: "assert.Never",
			Duration:  20 * time.Millisecond,
		}: 1,
		{
			Path:      "internal/federation/runner_postgres_test.go",
			Function:  "TestPostgresFederationRunnersElectOneLeaderPerSchema",
			Assertion: "assert.Never",
			Duration:  300 * time.Millisecond,
		}: 1,
		{
			Path:      "internal/vector/postgres_e2e_test.go",
			Function:  "TestPostgresReconcilersElectOneLeaderPerSchema",
			Assertion: "assert.Never",
			Duration:  300 * time.Millisecond,
		}: 1,
	}
}

func run(args []string, stderr io.Writer, allowed map[budgetKey]int) int {
	if len(args) > 1 {
		_, _ = fmt.Fprintln(stderr, "usage: timingbudgetcheck [directory]")
		return 2
	}
	root := "."
	if len(args) == 1 {
		root = args[0]
	}
	diagnostics, err := check(root, allowed)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "timingbudgetcheck: %v\n", err)
		return 2
	}
	for _, item := range diagnostics {
		_, _ = fmt.Fprintln(stderr, item.Text)
	}
	if len(diagnostics) > 0 {
		return 1
	}
	return 0
}

func check(root string, allowed map[budgetKey]int) ([]diagnostic, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: expected a directory", root)
	}
	walkRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	normalizedAllowed := make(map[budgetKey]int, len(allowed))
	for key, count := range allowed {
		key.Path = filepath.ToSlash(filepath.Clean(key.Path))
		normalizedAllowed[key] = count
	}

	var occurrences []budgetOccurrence
	err = filepath.WalkDir(walkRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != walkRoot && skippedDirectory(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		relativePath, relErr := filepath.Rel(walkRoot, path)
		if relErr != nil {
			return relErr
		}
		fileOccurrences, fileErr := checkFile(path, filepath.ToSlash(relativePath))
		if fileErr != nil {
			return fileErr
		}
		occurrences = append(occurrences, fileOccurrences...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(occurrences, func(i, j int) bool {
		left, right := occurrences[i], occurrences[j]
		if left.Key.Path != right.Key.Path {
			return left.Key.Path < right.Key.Path
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Key.Function != right.Key.Function {
			return left.Key.Function < right.Key.Function
		}
		return left.Key.Assertion < right.Key.Assertion
	})

	seen := make(map[budgetKey]int, len(occurrences))
	diagnostics := make([]diagnostic, 0)
	for _, occurrence := range occurrences {
		key := occurrence.Key
		allowedCount, isAllowed := normalizedAllowed[key]
		seen[key]++
		if isAllowed {
			if seen[key] > allowedCount {
				diagnostics = append(diagnostics, diagnostic{
					Path: key.Path,
					Line: occurrence.Line,
					Text: fmt.Sprintf("%s:%d: %s budget %s is an extra occurrence; allowance permits %d", key.Path, occurrence.Line, key.Assertion, key.Duration, allowedCount),
				})
			}
			continue
		}
		diagnostics = append(diagnostics, diagnostic{
			Path: key.Path,
			Line: occurrence.Line,
			Text: fmt.Sprintf("%s:%d: %s budget %s is below 1s in %s", key.Path, occurrence.Line, key.Assertion, key.Duration, key.Function),
		})
	}

	for key, allowedCount := range normalizedAllowed {
		if seen[key] < allowedCount {
			diagnostics = append(diagnostics, diagnostic{
				Path: key.Path,
				Text: fmt.Sprintf("%s: allowance %s budget %s for %s is stale", key.Path, key.Assertion, key.Duration, key.Function),
			})
		}
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Line == 0 && right.Line != 0 {
			return false
		}
		if left.Line != 0 && right.Line == 0 {
			return true
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Text < right.Text
	})
	return diagnostics, nil
}

func skippedDirectory(name string) bool {
	return name == "vendor" || name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func checkFile(path, relativePath string) ([]budgetOccurrence, error) {
	source, err := os.ReadFile(path) //nolint:gosec // G304: path comes from the scoped source walk.
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relativePath, source, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	imports, timeImport := importAliases(file)
	info := typeInfo(file, fset)
	occurrences := make([]budgetOccurrence, 0)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		functionName := ""
		if function.Name != nil {
			functionName = function.Name.Name
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 4 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isPollingAssertion(selector.Sel.Name) {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath, ok := importedQualifier(qualifier, imports, info)
			if !ok || (importPath != assertImport && importPath != requireImport) {
				return true
			}
			expression, known := literalExpression(call.Args[2], timeImport, info)
			if !known {
				return true
			}
			duration, known := evaluateDuration(expression)
			if !known || duration >= time.Second {
				return true
			}
			packageName := "assert"
			if importPath == requireImport {
				packageName = "require"
			}
			occurrences = append(occurrences, budgetOccurrence{
				Key: budgetKey{
					Path:      relativePath,
					Function:  functionName,
					Assertion: packageName + "." + selector.Sel.Name,
					Duration:  duration,
				},
				Line: fset.Position(call.Pos()).Line,
			})
			return true
		})
	}
	return occurrences, nil
}

func importAliases(file *ast.File) (map[string]string, string) {
	imports := make(map[string]string)
	timeImport := ""
	for _, specification := range file.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(importPath)
		if specification.Name != nil {
			name = specification.Name.Name
		}
		if name == "." || name == "_" {
			continue
		}
		switch importPath {
		case assertImport, requireImport:
			imports[name] = importPath
		case "time":
			timeImport = name
		}
	}
	return imports, timeImport
}

func typeInfo(file *ast.File, fset *token.FileSet) *types.Info {
	info := &types.Info{Uses: make(map[*ast.Ident]types.Object)}
	checker := types.Config{Importer: importer.Default()}
	_, _ = checker.Check(file.Name.Name, fset, []*ast.File{file}, info)
	return info
}

func importedQualifier(identifier *ast.Ident, imports map[string]string, info *types.Info) (string, bool) {
	if info != nil {
		if object, ok := info.Uses[identifier]; ok {
			packageName, ok := object.(*types.PkgName)
			if !ok {
				return "", false
			}
			return packageName.Imported().Path(), true
		}
	}
	if identifier.Obj != nil {
		return "", false
	}
	importPath, ok := imports[identifier.Name]
	return importPath, ok
}

func isPollingAssertion(name string) bool {
	switch name {
	case "Eventually", "Eventuallyf", "EventuallyWithT", "EventuallyWithTf", "Never", "Neverf":
		return true
	default:
		return false
	}
}

func literalExpression(expr ast.Expr, timeImport string, info *types.Info) (string, bool) {
	switch expression := expr.(type) {
	case *ast.BasicLit:
		return expression.Value, expression.Kind == token.INT || expression.Kind == token.FLOAT
	case *ast.ParenExpr:
		inner, ok := literalExpression(expression.X, timeImport, info)
		return "(" + inner + ")", ok
	case *ast.UnaryExpr:
		if expression.Op != token.ADD && expression.Op != token.SUB && expression.Op != token.XOR {
			return "", false
		}
		inner, ok := literalExpression(expression.X, timeImport, info)
		return "(" + expression.Op.String() + inner + ")", ok
	case *ast.BinaryExpr:
		switch expression.Op {
		case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.AND, token.OR, token.XOR, token.SHL, token.SHR, token.AND_NOT:
			left, leftOK := literalExpression(expression.X, timeImport, info)
			right, rightOK := literalExpression(expression.Y, timeImport, info)
			return "(" + left + " " + expression.Op.String() + " " + right + ")", leftOK && rightOK
		}
	case *ast.SelectorExpr:
		qualifier, ok := expression.X.(*ast.Ident)
		if !ok || !isTimeQualifier(qualifier, timeImport, info) {
			return "", false
		}
		units := map[string]struct{}{
			"Nanosecond":  {},
			"Microsecond": {},
			"Millisecond": {},
			"Second":      {},
			"Minute":      {},
			"Hour":        {},
		}
		if _, ok := units[expression.Sel.Name]; !ok {
			return "", false
		}
		return "time." + expression.Sel.Name, true
	case *ast.CallExpr:
		selector, ok := expression.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Duration" || len(expression.Args) != 1 || expression.Ellipsis.IsValid() {
			return "", false
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok || !isTimeQualifier(qualifier, timeImport, info) {
			return "", false
		}
		inner, ok := literalExpression(expression.Args[0], timeImport, info)
		return "time.Duration(" + inner + ")", ok
	}
	return "", false
}

func isTimeQualifier(identifier *ast.Ident, timeImport string, info *types.Info) bool {
	if timeImport == "" {
		return false
	}
	if info != nil {
		if object, ok := info.Uses[identifier]; ok {
			packageName, ok := object.(*types.PkgName)
			return ok && packageName.Imported().Path() == "time"
		}
	}
	return identifier.Obj == nil && identifier.Name == timeImport
}

func evaluateDuration(expression string) (time.Duration, bool) {
	timeImporter := importer.Default()
	if timeImporter == nil {
		return 0, false
	}
	timePackage, err := timeImporter.Import("time")
	if err != nil {
		return 0, false
	}
	pkg := types.NewPackage("fixture", "fixture")
	pkg.Scope().Insert(types.NewPkgName(token.NoPos, pkg, "time", timePackage))
	value, err := types.Eval(token.NewFileSet(), pkg, token.NoPos, expression)
	if err != nil || value.Value == nil {
		return 0, false
	}
	nanoseconds, ok := constant.Int64Val(constant.ToInt(value.Value))
	return time.Duration(nanoseconds), ok
}
