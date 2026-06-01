package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

// fkColumnQuerier abstracts what FKColumnResolver needs from either
// a *sql.DB or a *sql.Tx so the resolver can be reused at both
// import-time (transaction) and preflight-time (read-only DB).
type fkColumnQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// FKColumnResolver caches PRAGMA foreign_key_list lookups so each
// child table is queried at most once per resolver lifetime.
// foreign_key_check returns one row per violation with an `fkid`
// column that is the index into foreign_key_list(<table>) — the
// resolver maps that pair to the human column name.
type FKColumnResolver struct {
	q     fkColumnQuerier
	cache map[string]map[int]string
}

// NewFKColumnResolver constructs a resolver bound to the given querier.
// Pass whatever connection you already hold: *sqlitestore.Store for the read-only
// preflight scan, or *sql.Tx for in-transaction import validation.
func NewFKColumnResolver(q fkColumnQuerier) *FKColumnResolver {
	return &FKColumnResolver{q: q, cache: map[string]map[int]string{}}
}

// SQLite identifier names from foreign_key_check are sourced from
// schema metadata, but we still validate before interpolating into
// the PRAGMA call to avoid relying on caller hygiene.
var safeIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Resolve returns the FK column name in `table` for the constraint
// at index `fkid` (the value foreign_key_check returns in its 4th
// column). Returns "" + nil if the FK has no seq=0 entry — caller
// should treat that as "unknown column" and fall back to "?".
func (r *FKColumnResolver) Resolve(ctx context.Context, table string, fkid int) (string, error) {
	if cached, ok := r.cache[table]; ok {
		col, ok := cached[fkid]
		if !ok {
			return "", nil
		}
		return col, nil
	}
	if !safeIdent.MatchString(table) {
		return "", fmt.Errorf("FKColumnResolver: unsafe table name %q", table)
	}
	rows, err := r.q.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%q)`, table))
	if err != nil {
		return "", fmt.Errorf("foreign_key_list(%s): %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	perTable := map[int]string{}
	for rows.Next() {
		var (
			id, seq                                                    int
			parentTable, fromCol, toCol, onUpdate, onDelete, matchType string
		)
		if err := rows.Scan(&id, &seq, &parentTable, &fromCol, &toCol, &onUpdate, &onDelete, &matchType); err != nil {
			return "", fmt.Errorf("scan foreign_key_list(%s): %w", table, err)
		}
		// Only record seq=0 — composite FKs (multi-column) would
		// have additional rows with the same id, but our schema
		// has no composite FKs and surfacing only the first column
		// is the right behavior even if one ever appears.
		if seq == 0 {
			perTable[id] = fromCol
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("foreign_key_list(%s) rows: %w", table, err)
	}
	r.cache[table] = perTable
	if col, ok := perTable[fkid]; ok {
		return col, nil
	}
	return "", nil
}
