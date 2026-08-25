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

// OrphanReport is the result of preflighting a source DB before cutover.
// Rows holds exactly one disposition per (child table, rowid): a row that is
// classified twice — an events row with both issue_id and related_issue_id
// orphaned produces two PRAGMA foreign_key_check rows — keeps the
// higher-precedence disposition, so it can never be both dropped and
// scrubbed. UnknownViolations is everything that doesn't match a known
// orphan class; a non-empty list halts the cutover.
type OrphanReport struct {
	Rows              map[orphanRow]orphanDisposition
	UnknownViolations []FKViolation
}

// orphanRow identifies one source row by child table and rowid.
type orphanRow struct {
	Table string
	RowID int64
}

func newOrphanReport() OrphanReport {
	return OrphanReport{Rows: map[orphanRow]orphanDisposition{}}
}

// record files disp against key, keeping the higher-precedence disposition
// when the same row is classified more than once.
func (r OrphanReport) record(key orphanRow, disp orphanDisposition) {
	if existing, ok := r.Rows[key]; ok && existing.rank() >= disp.rank() {
		return
	}
	r.Rows[key] = disp
}

// DropCount reports how many rows of table the export discards.
func (r OrphanReport) DropCount(table string) int {
	return r.countByDisposition(table, dispositionDrop)
}

// ScrubCount reports how many rows of table survive the export with their
// orphan peer columns NULLed.
func (r OrphanReport) ScrubCount(table string) int {
	return r.countByDisposition(table, dispositionScrub)
}

func (r OrphanReport) countByDisposition(table string, want orphanDisposition) int {
	n := 0
	for row, disp := range r.Rows {
		if row.Table == table && disp == want {
			n++
		}
	}
	return n
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

// rank orders dispositions by precedence: when one row is classified twice,
// the higher rank wins. Dropping the row makes scrubbing it moot, so drop
// outranks scrub. This is deliberately independent of the iota order above,
// which lists drop before scrub — comparing the constants directly would
// invert the rule.
func (d orphanDisposition) rank() int {
	switch d {
	case dispositionDrop:
		return 2
	case dispositionScrub:
		return 1
	default:
		return 0
	}
}

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
// Drop precedence: when the same rowid is classified twice during the
// scan, the drop disposition wins regardless of arrival order. The source
// DB is not modified.
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

	report := newOrphanReport()
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
		// A NULL rowid (WITHOUT ROWID source table) gives us no stable row
		// identity, so the row cannot be filed under a disposition without
		// risking coalescing two distinct rows. The four known orphan
		// classes are all rowid tables, so this should never fire on real
		// data; when it does, surface the violation rather than guess.
		if disp == dispositionUnknown || !r.RowID.Valid {
			report.UnknownViolations = append(report.UnknownViolations, FKViolation{
				Table:       r.Table,
				RowID:       r.RowID,
				ParentTable: r.ParentTable,
				Column:      col,
			})
			continue
		}
		report.record(orphanRow{Table: r.Table, RowID: r.RowID.Int64}, disp)
	}
	return report, nil
}
