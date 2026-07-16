// Stubgen emits internal/db/pgstore/stubs_gen.go from the db.Storage
// interface declared in internal/db/storage.go. Methods listed in
// alreadyImplemented are skipped so this binary's output is gofmt-equivalent
// to the file checked in next to it. Run from internal/db/pgstore as:
//
//	go run ./stubgen
//
// or from anywhere in the module tree as:
//
//	go generate ./internal/db/pgstore
//
// When db.Storage grows, the workflow for replacing the generated guard is:
//  1. Add the method name to alreadyImplemented below,
//  2. Write the real implementation in a new pgstore .go file,
//  3. Re-run go generate so the now-implemented method drops out of stubs_gen.go.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"sort"
	"strings"
)

const (
	interfaceName = "Storage"
	receiverType  = "Store"
	receiverVar   = "s"
	sentinelName  = "ErrNotImplemented"
	qualifierPkg  = "db"
)

// alreadyImplemented lists Storage methods implemented on *pgstore.Store in
// store.go and open.go, plus Close which is satisfied by the embedded
// *sql.DB. Each method that lands a real query gets added here, and
// the generator's output for that method disappears on the next regenerate.
var alreadyImplemented = map[string]bool{
	"ActiveFederationQuarantine":           true, // federation_quarantine.go
	"ActiveFederationQuarantinesByProject": true, // federation_quarantine.go
	"AdoptProjectIntoFederation":           true, // federation_adoption.go
	"AdvanceFederationPullCursor":          true, // federation_control.go
	"AdvanceFederationPushCursor":          true, // federation_control.go
	"AuthorizeFederationToken":             true, // federation_control.go
	"ClearFederationSyncError":             true, // federation_control.go
	"CountActiveFederationEnrollments":     true, // federation_control.go
	"CreateFederationEnrollment":           true, // federation_control.go
	"EnableFederationPush":                 true, // federation_control.go
	"EnableProjectFederation":              true, // federation_projection.go
	"ExportFederationBindings":             true, // export.go
	"ExportFederationEnrollments":          true, // export.go
	"ExportFederationQuarantine":           true, // export.go
	"ExportFederationSyncStatus":           true, // export.go
	"FederationBindingByProject":           true, // federation_control.go
	"FederationSyncStatusByProject":        true, // federation_control.go
	"ListFederationBindings":               true, // federation_control.go
	"ListFederationEnrollments":            true, // federation_control.go
	"LeaveFederationReplica":               true, // federation_projection.go
	"MaterializeFederatedProject":          true, // federation_projection.go
	"RecordFederationQuarantine":           true, // federation_quarantine.go
	"RecordFederationSyncError":            true, // federation_control.go
	"RecordFederationSyncPullStarted":      true, // federation_control.go
	"RecordFederationSyncPullSuccess":      true, // federation_control.go
	"RecordFederationSyncPushStarted":      true, // federation_control.go
	"RecordFederationSyncPushSuccess":      true, // federation_control.go
	"RecordFederationSyncReset":            true, // federation_control.go
	"RefreshProjectFederationBaseline":     true, // federation_projection.go
	"RetryFederationQuarantine":            true, // federation_quarantine.go
	"RevokeFederationEnrollment":           true, // federation_control.go
	"SkipFederationQuarantine":             true, // federation_quarantine.go
	"UpsertFederationBinding":              true, // federation_control.go
	"AliasByID":                            true, // aliases.go
	"AliasByIdentity":                      true, // aliases.go
	"AddLabel":                             true, // labels.go
	"AddLabelAndEvent":                     true, // labels.go
	"ActivelyBlockedIssueIDs":              true, // relationship_queries.go
	"AcquireClaim":                         true, // claims_core.go
	"AcquireIdempotencyLock":               true, // idempotency_lock.go
	"ApplyClaimStatus":                     true, // claims_pending.go
	"AttachAlias":                          true, // aliases.go
	"BatchProjectStats":                    true, // project_lifecycle.go
	"BlockNumbersByIssues":                 true, // relationship_queries.go
	"BlockedByNumbersByIssues":             true, // relationship_queries.go
	"ChildCountsByParents":                 true, // relationship_queries.go
	"ChildrenOfIssue":                      true, // relationship_queries.go
	"ClaimIssueSyncBinding":                true, // issue_sync.go
	"ClaimOwner":                           true, // issue_lifecycle.go
	"ClaimStatus":                          true, // claims_core.go
	"ClaimStatusReadOnly":                  true, // claims_core.go
	"ClaimStatusRefreshError":              true, // claims_pending.go
	"CheckClaimGate":                       true, // claims_pending.go
	"ClearClaimStatusRefreshError":         true, // claims_pending.go
	"Close":                                true, // inherited from embedded *sql.DB
	"CloseIssue":                           true, // issue_lifecycle.go
	"CloseIssueWithEvents":                 true, // issue_lifecycle.go
	"CommentBodyByID":                      true, // comments.go
	"CommentsByIssue":                      true, // comments.go
	"CountOpenIssues":                      true, // project_lifecycle.go
	"CountLiveClaims":                      true, // claims_core.go
	"CountPendingClaims":                   true, // claims_core.go
	"CreateAPIToken":                       true, // tokens.go
	"CreateComment":                        true, // comments.go
	"CreateIssue":                          true, // issues.go
	"CreateLink":                           true, // links.go
	"CreateLinkAndEvent":                   true, // links.go
	"CreateProject":                        true, // projects.go
	"CreateProjectWithUID":                 true, // projects.go
	"CreateRecurrence":                     true, // recurrences.go
	"DetachProjectAlias":                   true, // project_lifecycle.go
	"DeleteLinkAndEvent":                   true, // links.go
	"DeleteLinkByID":                       true, // links.go
	"DisableIssueSyncBinding":              true, // issue_sync.go
	"EditComment":                          true, // comments.go
	"EditIssue":                            true, // issue_lifecycle.go
	"EditIssueAtomic":                      true, // atomic_edit.go
	"EnsureSystemProject":                  true, // tokens.go
	"EnqueuePendingClaim":                  true, // claims_pending.go
	"EventsAfter":                          true, // events.go
	"EventsByUIDs":                         true, // events.go
	"EventsInWindow":                       true, // events.go
	"ExpireTimedClaims":                    true, // claims_core.go
	"ExpireTimedClaimsForProject":          true, // claims_core.go
	"ExportComments":                       true, // export.go
	"ExportEvents":                         true, // export.go
	"ExportImportMappings":                 true, // export.go
	"ExportIssueClaims":                    true, // export.go
	"ExportIssueLabels":                    true, // export.go
	"ExportIssueSyncBindings":              true, // export.go
	"ExportIssueSyncStatus":                true, // export.go
	"ExportIssues":                         true, // export.go
	"ExportLinks":                          true, // export.go
	"ExportMeta":                           true, // export.go
	"ExportPendingClaimRequests":           true, // export.go
	"ExportProjectPurgeLog":                true, // export.go
	"ExportProjectAliases":                 true, // export.go
	"ExportProjects":                       true, // export.go
	"ExportPurgeLog":                       true, // export.go
	"ExportRecurrences":                    true, // export.go
	"ExportSequences":                      true, // export.go
	"GetRecurrenceByID":                    true, // recurrences.go
	"ForceReleaseClaim":                    true, // claims_core.go
	"GetRecurrenceByUID":                   true, // recurrences.go
	"HardDeleteProject":                    true, // aliases.go
	"HasLabel":                             true, // labels.go
	"ImportBatch":                          true, // imports.go
	"ImportReplay":                         true, // import_replay.go
	"InstanceUID":                          true, // store.go
	"InsertCloseThrottledEvent":            true, // events.go
	"IngestFederationEvents":               true, // federation_ingest.go
	"InsertRemoteEvent":                    true, // federation_events.go
	"IssueByID":                            true, // issues.go
	"IssueByShortID":                       true, // issues.go
	"IssueByUID":                           true, // issues.go
	"IssueQualifiersByUIDs":                true, // discovery.go
	"ImportMappingBySource":                true, // import_mappings.go
	"ImportMappingsByProjectSource":        true, // import_mappings.go
	"IssueSyncBindingByID":                 true, // issue_sync.go
	"IssueSyncBindingByProject":            true, // issue_sync.go
	"IssueSyncStatusByProject":             true, // issue_sync.go
	"IssueUIDPrefixMatch":                  true, // issues.go
	"LatestAliasForProject":                true, // aliases.go
	"LabelByEndpoints":                     true, // labels.go
	"LabelCounts":                          true, // labels.go
	"LabelsByIssue":                        true, // labels.go
	"LabelsByIssues":                       true, // labels.go
	"LabelsForIssue":                       true, // labels.go
	"ListAPITokens":                        true, // tokens.go
	"ListAllIssues":                        true, // issues.go
	"ListDueIssueSyncBindings":             true, // issue_sync.go
	"ListIssueContent":                     true, // discovery.go
	"ListIssues":                           true, // issues.go
	"ListPendingClaimRequests":             true, // claims_pending.go
	"ListPendingClaimRequestsForIssue":     true, // claims_pending.go
	"ListProjects":                         true, // projects.go
	"ListProjectsIncludingArchived":        true, // projects.go
	"ListRecurrencesByProject":             true, // recurrences.go
	"LinkByEndpoints":                      true, // links.go
	"LinkByID":                             true, // links.go
	"LinksByIssue":                         true, // links.go
	"Path":                                 true, // store.go
	"LookupIdempotency":                    true, // idempotency.go
	"MaxEventID":                           true, // events.go
	"MaxFederationBaselineEventID":         true, // events.go
	"MaxLocalOriginEventID":                true, // events.go
	"MarkClaimStatusRefreshError":          true, // claims_pending.go
	"MarkPendingClaimAttempt":              true, // claims_pending.go
	"MaterializeNext":                      true, // recurrences.go
	"MergeProjects":                        true, // project_merge.go
	"MoveIssueProject":                     true, // project_move.go
	"OpenChildrenOf":                       true, // relationship_queries.go
	"ParentNumbersByIssues":                true, // relationship_queries.go
	"ParentOf":                             true, // links.go
	"ParentShortIDsByIssues":               true, // relationship_queries.go
	"PatchIssueMetadata":                   true, // metadata.go
	"PatchProjectMetadata":                 true, // metadata.go
	"PatchRecurrence":                      true, // recurrences.go
	"PendingFederationPushEvents":          true, // federation_events.go
	"PendingFederationPushStats":           true, // federation_events.go
	"ProjectByID":                          true, // projects.go
	"ProjectByName":                        true, // projects.go
	"ProjectByNameIncludingArchived":       true, // projects.go
	"ProjectByUID":                         true, // projects.go
	"ProjectAliases":                       true, // aliases.go
	"PurgeIssue":                           true, // purge.go
	"PurgeProject":                         true, // project_lifecycle.go
	"PurgeResetCheck":                      true, // purge.go
	"ReadyIssues":                          true, // discovery.go
	"ReadyIssuesGlobal":                    true, // discovery.go
	"RecentSameMessageClose":               true, // events.go
	"RecentSiblingCloses":                  true, // events.go
	"ReconcileLocalFederationEcho":         true, // federation_events.go
	"RecordIssueSyncError":                 true, // issue_sync.go
	"RecordIssueSyncSuccess":               true, // issue_sync.go
	"RefreshInstanceUID":                   true, // store.go
	"RefreshIssueSyncBinding":              true, // issue_sync.go
	"RejectPendingClaim":                   true, // claims_pending.go
	"ReassignAlias":                        true, // aliases.go
	"RemoveLabel":                          true, // labels.go
	"RemoveLabelAndEvent":                  true, // labels.go
	"RemoveProject":                        true, // project_lifecycle.go
	"ReleaseClaim":                         true, // claims_core.go
	"RenewClaim":                           true, // claims_core.go
	"RenameProject":                        true, // projects.go
	"RelatedNumbersByIssues":               true, // relationship_queries.go
	"ReopenIssue":                          true, // issue_lifecycle.go
	"ResolveAPIToken":                      true, // tokens.go
	"ResolvePendingClaim":                  true, // claims_pending.go
	"ResetFederatedProject":                true, // federation_reset.go
	"ResetFederatedProjectIfNoPendingPush": true, // federation_reset.go
	"RewriteAuthorIdentity":                true, // comments.go
	"RestoreIssue":                         true, // issue_lifecycle.go
	"RestoreProject":                       true, // project_lifecycle.go
	"RetryTransient":                       true, // store.go
	"RevokeAPIToken":                       true, // tokens.go
	"SchemaVersion":                        true, // store.go
	"SearchFTS":                            true, // search.go
	"SearchFTSAny":                         true, // search.go
	"SoftDeleteIssue":                      true, // issue_lifecycle.go
	"SoftDeleteRecurrence":                 true, // recurrences.go
	"SystemProject":                        true, // tokens.go
	"UnresolvedClaimViolationsForIssue":    true, // claim_violations.go
	"UnresolvedClaimViolationsForProject":  true, // claim_violations.go
	"UpdateOwner":                          true, // issue_lifecycle.go
	"UpdatePriority":                       true, // issue_lifecycle.go
	"UpsertClaimCache":                     true, // claims_pending.go
	"UpsertImportMapping":                  true, // import_mappings.go
	"UpsertIssueSyncBinding":               true, // issue_sync.go
}

func main() {
	in := flag.String("in", "../storage.go", "path to storage.go (relative to working dir)")
	out := flag.String("out", "stubs_gen.go", "output path")
	check := flag.Bool("check", false, "exit non-zero if -out differs from regenerated content (does not write)")
	flag.Parse()

	src, err := Generate(*in)
	if err != nil {
		log.Fatal(err)
	}

	if *check {
		existing, readErr := os.ReadFile(*out)
		if readErr != nil {
			log.Fatalf("read existing %s: %v", *out, readErr)
		}
		if !bytes.Equal(existing, src) {
			log.Fatalf("%s is out of date — run `go generate ./internal/db/pgstore` to regenerate", *out)
		}
		return
	}

	// stubs_gen.go is a generated Go source file committed to the repo
	// alongside hand-written .go files; 0o644 matches gofmt/goimports output
	// and is the canonical mode for source under VCS.
	if err := os.WriteFile(*out, src, 0o644); err != nil { //nolint:gosec // generated source file, not a credential
		log.Fatal(err)
	}
}

// Generate parses storagePath, finds the Storage interface, and returns
// gofmt-formatted source for the pgstore stubs file. Methods in
// alreadyImplemented are skipped — they have real implementations elsewhere.
func Generate(storagePath string) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, storagePath, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", storagePath, err)
	}

	iface := findStorageInterface(file)
	if iface == nil {
		return nil, fmt.Errorf("interface %q not found in %s", interfaceName, storagePath)
	}

	methods, err := collectStubMethods(iface)
	if err != nil {
		return nil, err
	}

	var stubs bytes.Buffer
	for _, m := range methods {
		emitStub(&stubs, fset, m)
	}
	var buf bytes.Buffer
	buf.WriteString(fileHeaderPrefix)
	for _, importPath := range []string{"context", "iter", "time"} {
		if strings.Contains(stubs.String(), importPath+".") {
			fmt.Fprintf(&buf, "\t%q\n", importPath)
		}
	}
	buf.WriteString("\n\t\"go.kenn.io/kata/internal/db\"\n)\n\n")
	buf.WriteString(fileHeaderSuffix)
	buf.Write(stubs.Bytes())

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w\n--- generator output:\n%s", err, buf.String())
	}
	return formatted, nil
}

func findStorageInterface(file *ast.File) *ast.InterfaceType {
	var found *ast.InterfaceType
	ast.Inspect(file, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != interfaceName {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		found = it
		return false
	})
	return found
}

type stubMethod struct {
	name string
	ft   *ast.FuncType
}

// MethodStatus records whether pgstore has a real implementation or a
// generated sentinel stub for one Storage method.
type MethodStatus string

const (
	MethodImplemented MethodStatus = "implemented"
	MethodStubbed     MethodStatus = "stubbed"
)

// StorageMethodInventory is one mechanically discovered Storage method and
// its current pgstore implementation state.
type StorageMethodInventory struct {
	Name   string
	Status MethodStatus
}

// CollectStorageMethodInventory parses Storage and classifies every method.
// It rejects embedded interfaces and stale allow-list entries so a future
// interface change cannot silently disappear from the parity accounting.
func CollectStorageMethodInventory(storagePath string) ([]StorageMethodInventory, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, storagePath, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", storagePath, err)
	}

	iface := findStorageInterface(file)
	if iface == nil {
		return nil, fmt.Errorf("interface %q not found in %s", interfaceName, storagePath)
	}

	seen := make(map[string]bool, len(iface.Methods.List))
	methods := make([]StorageMethodInventory, 0, len(iface.Methods.List))
	for _, field := range iface.Methods.List {
		if len(field.Names) == 0 {
			return nil, fmt.Errorf("embedded interfaces are not supported in %s", interfaceName)
		}
		if _, ok := field.Type.(*ast.FuncType); !ok {
			return nil, fmt.Errorf("method %s: expected FuncType, got %T", field.Names[0].Name, field.Type)
		}
		for _, ident := range field.Names {
			status := MethodStubbed
			if alreadyImplemented[ident.Name] {
				status = MethodImplemented
			}
			methods = append(methods, StorageMethodInventory{Name: ident.Name, Status: status})
			seen[ident.Name] = true
		}
	}
	for name := range alreadyImplemented {
		if !seen[name] {
			return nil, fmt.Errorf("implemented method %q is not declared by %s", name, interfaceName)
		}
	}

	sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
	return methods, nil
}

func collectStubMethods(iface *ast.InterfaceType) ([]stubMethod, error) {
	var out []stubMethod
	for _, f := range iface.Methods.List {
		if len(f.Names) == 0 {
			// embedded interfaces — none today
			continue
		}
		name := f.Names[0].Name
		if alreadyImplemented[name] {
			continue
		}
		ft, ok := f.Type.(*ast.FuncType)
		if !ok {
			return nil, fmt.Errorf("method %s: expected FuncType, got %T", name, f.Type)
		}
		out = append(out, stubMethod{name: name, ft: ft})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func emitStub(w *bytes.Buffer, fset *token.FileSet, m stubMethod) {
	fmt.Fprintf(w, "func (%s *%s) %s(", receiverVar, receiverType, m.name)
	emitParams(w, fset, m.ft.Params)
	w.WriteString(") ")
	results := flattenResults(m.ft.Results)
	emitReturnSig(w, fset, results)
	w.WriteString(" {\n")
	emitBody(w, fset, results)
	w.WriteString("}\n\n")
}

func emitParams(w *bytes.Buffer, fset *token.FileSet, params *ast.FieldList) {
	if params == nil {
		return
	}
	first := true
	for _, p := range params.List {
		n := len(p.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			if !first {
				w.WriteString(", ")
			}
			first = false
			w.WriteString("_ ")
			writeQualifiedType(w, fset, p.Type)
		}
	}
}

func flattenResults(results *ast.FieldList) []ast.Expr {
	if results == nil {
		return nil
	}
	var out []ast.Expr
	for _, r := range results.List {
		n := len(r.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, r.Type)
		}
	}
	return out
}

func emitReturnSig(w *bytes.Buffer, fset *token.FileSet, results []ast.Expr) {
	if len(results) == 0 {
		return
	}
	parens := len(results) > 1
	if parens {
		w.WriteString("(")
	}
	for i, r := range results {
		if i > 0 {
			w.WriteString(", ")
		}
		writeQualifiedType(w, fset, r)
	}
	if parens {
		w.WriteString(")")
	}
}

func emitBody(w *bytes.Buffer, fset *token.FileSet, results []ast.Expr) {
	if inner := iterSeq2Inner(results); inner != nil {
		// iter.Seq2[T, error] — emit a function that yields once with the sentinel
		w.WriteString("\treturn func(yield func(")
		writeQualifiedType(w, fset, inner)
		w.WriteString(", error) bool) {\n")
		w.WriteString("\t\tyield(")
		writeZeroValue(w, fset, inner)
		w.WriteString(", ")
		w.WriteString(sentinelName)
		w.WriteString(")\n\t}\n")
		return
	}
	if len(results) == 0 {
		return
	}
	if len(results) == 1 && isErrorIdent(results[0]) {
		w.WriteString("\treturn ")
		w.WriteString(sentinelName)
		w.WriteString("\n")
		return
	}
	if !isErrorIdent(results[len(results)-1]) {
		// Unexpected — emit a panic so a future signature change surfaces
		// instead of silently returning zero values without an error.
		w.WriteString("\tpanic(\"stubgen: unsupported return shape — last result is not error\")\n")
		return
	}
	w.WriteString("\treturn ")
	for i, r := range results {
		if i > 0 {
			w.WriteString(", ")
		}
		if i == len(results)-1 {
			w.WriteString(sentinelName)
		} else {
			writeZeroValue(w, fset, r)
		}
	}
	w.WriteString("\n")
}

// writeQualifiedType writes e to w, qualifying bare identifiers (that aren't
// builtins) with the qualifierPkg prefix so types declared in the db package
// resolve correctly when the file is generated into pgstore.
func writeQualifiedType(w *bytes.Buffer, fset *token.FileSet, e ast.Expr) {
	switch t := e.(type) {
	case *ast.Ident:
		if isBuiltin(t.Name) {
			w.WriteString(t.Name)
		} else {
			w.WriteString(qualifierPkg)
			w.WriteString(".")
			w.WriteString(t.Name)
		}
	case *ast.SelectorExpr:
		// X is a package qualifier (context, iter, time, etc.); emit verbatim
		// rather than recursing — recursion would mis-qualify "context" as
		// "db.context".
		if pkg, ok := t.X.(*ast.Ident); ok {
			w.WriteString(pkg.Name)
		} else {
			writeQualifiedType(w, fset, t.X)
		}
		w.WriteString(".")
		w.WriteString(t.Sel.Name)
	case *ast.StarExpr:
		w.WriteString("*")
		writeQualifiedType(w, fset, t.X)
	case *ast.ArrayType:
		if t.Len != nil {
			// Fixed-size [N]T isn't used by Storage today; fall back to printer.
			_ = printer.Fprint(w, fset, t)
			return
		}
		w.WriteString("[]")
		writeQualifiedType(w, fset, t.Elt)
	case *ast.MapType:
		w.WriteString("map[")
		writeQualifiedType(w, fset, t.Key)
		w.WriteString("]")
		writeQualifiedType(w, fset, t.Value)
	case *ast.FuncType:
		// Only RetryTransient has a func-typed param and it's in the
		// alreadyImplemented skiplist. Fall back to printer for safety.
		_ = printer.Fprint(w, fset, t)
	case *ast.InterfaceType:
		w.WriteString("interface{}")
	case *ast.ChanType:
		switch t.Dir {
		case ast.SEND:
			w.WriteString("chan<- ")
		case ast.RECV:
			w.WriteString("<-chan ")
		default:
			w.WriteString("chan ")
		}
		writeQualifiedType(w, fset, t.Value)
	case *ast.IndexExpr:
		writeQualifiedType(w, fset, t.X)
		w.WriteString("[")
		writeQualifiedType(w, fset, t.Index)
		w.WriteString("]")
	case *ast.IndexListExpr:
		writeQualifiedType(w, fset, t.X)
		w.WriteString("[")
		for i, idx := range t.Indices {
			if i > 0 {
				w.WriteString(", ")
			}
			writeQualifiedType(w, fset, idx)
		}
		w.WriteString("]")
	case *ast.Ellipsis:
		w.WriteString("...")
		writeQualifiedType(w, fset, t.Elt)
	default:
		_ = printer.Fprint(w, fset, e)
	}
}

func writeZeroValue(w *bytes.Buffer, fset *token.FileSet, e ast.Expr) {
	switch t := e.(type) {
	case *ast.StarExpr, *ast.ArrayType, *ast.MapType, *ast.FuncType, *ast.ChanType, *ast.InterfaceType:
		w.WriteString("nil")
	case *ast.Ident:
		switch t.Name {
		case "string":
			w.WriteString(`""`)
		case "bool":
			w.WriteString("false")
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
			"byte", "rune", "float32", "float64", "complex64", "complex128":
			w.WriteString("0")
		case "error", "any":
			w.WriteString("nil")
		default:
			w.WriteString(qualifierPkg)
			w.WriteString(".")
			w.WriteString(t.Name)
			w.WriteString("{}")
		}
	default:
		writeQualifiedType(w, fset, e)
		w.WriteString("{}")
	}
}

func isBuiltin(name string) bool {
	switch name {
	case "string", "bool", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"byte", "rune", "float32", "float64", "complex64", "complex128",
		"error", "any":
		return true
	}
	return false
}

func isErrorIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "error"
}

// iterSeq2Inner returns the T expression if results is a single
// iter.Seq2[T, error], else nil.
func iterSeq2Inner(results []ast.Expr) ast.Expr {
	if len(results) != 1 {
		return nil
	}
	ile, ok := results[0].(*ast.IndexListExpr)
	if !ok {
		return nil
	}
	sel, ok := ile.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Seq2" {
		return nil
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "iter" {
		return nil
	}
	if len(ile.Indices) != 2 {
		return nil
	}
	return ile.Indices[0]
}

const fileHeaderPrefix = `// Code generated by ./stubgen; DO NOT EDIT.
//
// This file is regenerated from internal/db/storage.go's Storage interface
// each time //go:generate runs. To regenerate after changing the interface
// or implementing a method elsewhere in this package:
//
//	go generate ./internal/db/pgstore
//
// or, from this directory:
//
//	go run ./stubgen
//
// Methods implemented in store.go, open.go, or inherited from the
// embedded *sql.DB are skipped via stubgen/main.go's alreadyImplemented
// allow-list. The checked-in file has no stubs at full parity. When Storage
// grows, complete the new backend method by:
//
//  1. Adding the method name to alreadyImplemented in stubgen/main.go,
//  2. Writing the real implementation in a new pgstore .go file, and
//  3. Re-running go generate so this file no longer carries the stub.

package pgstore

import (
`

const fileHeaderSuffix = `
// Compile-time check that *Store satisfies db.Storage. The assertion stays
// here so a missing or mistyped method is caught at build time; the inventory
// test separately requires every method to have shared conformance coverage.
var _ db.Storage = (*Store)(nil)

`
