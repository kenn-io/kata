package jsonl

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// knownOrphanClasses is the ordered list of child tables whose
// orphans cutover knows how to handle, used for the cutover stderr
// summary's display order. Keep this in sync with the classifyKnownOrphan
// switch below: every table here must have at least one drop or scrub
// case there, and every table classifyKnownOrphan handles must appear
// here. The order is the order classes are listed in the summary line.
var knownOrphanClasses = []string{"events", "comments", "links", "issue_labels"}

// OrphanReport is the result of preflighting a source DB before
// cutover. DroppedRowsByTable and ScrubbedRowsByTable are keyed by
// child-table name; values are the set of rowids in each
// disposition. UnknownViolations is everything that doesn't match
// a known orphan class — a non-empty list halts the cutover.
type OrphanReport struct {
	DroppedRowsByTable  map[string]map[int64]struct{}
	ScrubbedRowsByTable map[string]map[int64]struct{}
	UnknownViolations   []FKViolation
}

// FKViolation is a single PRAGMA foreign_key_check row with the
// fkid resolved to a column name. RowID is sql.NullInt64 because
// PRAGMA foreign_key_check returns NULL for the rowid column on
// WITHOUT ROWID tables; scanning into a plain int64 would fail.
type FKViolation struct {
	Table       string
	RowID       sql.NullInt64
	ParentTable string
	Column      string
}

// orphanDisposition captures whether a known-class violation
// causes the row to be dropped at export or merely scrubbed.
type orphanDisposition int

const (
	dispositionUnknown orphanDisposition = iota
	dispositionDrop
	dispositionScrub
)

// classifyKnownOrphan returns dispositionDrop or dispositionScrub
// for known issue-child orphan classes, or dispositionUnknown
// otherwise. Keep this in sync with knownOrphanClasses above, with
// the disposition table in the design doc, and with the export-side
// scrub logic in export.go.
func classifyKnownOrphan(table, parent, column string) orphanDisposition {
	if parent != "issues" {
		return dispositionUnknown
	}
	switch table {
	case "comments":
		if column == "issue_id" {
			return dispositionDrop
		}
	case "links":
		if column == "from_issue_id" || column == "to_issue_id" {
			return dispositionDrop
		}
	case "issue_labels":
		if column == "issue_id" {
			return dispositionDrop
		}
	case "events":
		if column == "issue_id" {
			return dispositionDrop
		}
		if column == "related_issue_id" {
			return dispositionScrub
		}
	}
	return dispositionUnknown
}

// PreflightSourceFKs opens path read-only, runs PRAGMA
// foreign_key_check, classifies each violation against the
// known-orphan-class table, and returns a structured report.
// Drop precedence: when the same rowid appears in both buckets
// during the scan, drop wins (the scrub entry is removed and any
// later scrub entry for that rowid is skipped). The source DB is
// not modified.
func PreflightSourceFKs(ctx context.Context, path string) (OrphanReport, error) {
	source, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	if err != nil {
		return OrphanReport{}, fmt.Errorf("preflight open: %w", err)
	}
	defer func() { _ = source.Close() }()

	rows, err := source.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return OrphanReport{}, fmt.Errorf("preflight foreign_key_check: %w", err)
	}
	type rawViol struct {
		Table       string
		RowID       sql.NullInt64
		ParentTable string
		FKID        int
	}
	var raws []rawViol
	for rows.Next() {
		var r rawViol
		if err := rows.Scan(&r.Table, &r.RowID, &r.ParentTable, &r.FKID); err != nil {
			_ = rows.Close()
			return OrphanReport{}, fmt.Errorf("preflight scan: %w", err)
		}
		raws = append(raws, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return OrphanReport{}, fmt.Errorf("preflight rows: %w", err)
	}
	_ = rows.Close()

	report := OrphanReport{
		DroppedRowsByTable:  map[string]map[int64]struct{}{},
		ScrubbedRowsByTable: map[string]map[int64]struct{}{},
	}
	resolver := sqlitestore.NewFKColumnResolver(source)
	for _, r := range raws {
		// Classification depends on the column name -- if we can't resolve
		// it, we cannot safely distinguish a known orphan class from an
		// unknown one. Abort rather than risk a misclassification that
		// would either falsely halt cutover or wrongly let it proceed.
		// (This intentionally differs from import.go's checkForeignKeyViolations,
		// which annotates the row and continues; that path is reporting
		// failures to a user, not making a go/no-go decision.)
		col, err := resolver.Resolve(ctx, r.Table, r.FKID)
		if err != nil {
			return OrphanReport{}, fmt.Errorf("preflight resolve %s: %w", r.Table, err)
		}
		disp := classifyKnownOrphan(r.Table, r.ParentTable, col)
		// A NULL rowid (WITHOUT ROWID source table) leaves us with no
		// stable identifier to dedupe drop/scrub buckets by, so we
		// can't safely include it in either. The four known orphan
		// classes are all rowid tables, so this should never fire on
		// real data, but if it ever does we surface the violation
		// rather than silently coalesce or skip it.
		if !r.RowID.Valid {
			disp = dispositionUnknown
		}
		switch disp {
		case dispositionDrop:
			ensureRowSet(report.DroppedRowsByTable, r.Table)[r.RowID.Int64] = struct{}{}
			// Drop precedence: remove any earlier scrub entry for
			// this rowid in the same table.
			if scrubs, ok := report.ScrubbedRowsByTable[r.Table]; ok {
				delete(scrubs, r.RowID.Int64)
			}
		case dispositionScrub:
			// Drop precedence: skip if this rowid is already in
			// the drop bucket for the same table.
			if drops, ok := report.DroppedRowsByTable[r.Table]; ok {
				if _, present := drops[r.RowID.Int64]; present {
					continue
				}
			}
			ensureRowSet(report.ScrubbedRowsByTable, r.Table)[r.RowID.Int64] = struct{}{}
		default:
			report.UnknownViolations = append(report.UnknownViolations, FKViolation{
				Table:       r.Table,
				RowID:       r.RowID,
				ParentTable: r.ParentTable,
				Column:      col,
			})
		}
	}
	// Trim empty per-table maps so callers can use len() to gate.
	for tbl, set := range report.DroppedRowsByTable {
		if len(set) == 0 {
			delete(report.DroppedRowsByTable, tbl)
		}
	}
	for tbl, set := range report.ScrubbedRowsByTable {
		if len(set) == 0 {
			delete(report.ScrubbedRowsByTable, tbl)
		}
	}
	return report, nil
}

func ensureRowSet(m map[string]map[int64]struct{}, key string) map[int64]struct{} {
	if existing, ok := m[key]; ok {
		return existing
	}
	fresh := map[int64]struct{}{}
	m[key] = fresh
	return fresh
}
