package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// inPlaceUpgrades add canonical tables, then canonical indexes. A table is
// added only when it starts empty, so no existing row needs converting.
var inPlaceUpgrades = []struct {
	From, To int
	Tables   []string
	Indexes  []string
}{
	{41, 42, nil, []string{"idx_issues_project_created", "idx_issues_created"}},
	{42, 43, nil, []string{"idx_events_resolved_issue_uid"}},
	{43, 44, nil, []string{"idx_events_reset_from"}},
	{44, 45, nil, []string{"idx_issues_uid_prefix", "idx_issues_project_uid_prefix"}},
	{45, 46, nil, []string{"idx_issues_project_ready_priority", "idx_issues_ready_priority"}},
	{46, 47, nil, []string{"idx_issues_ready_updated"}},
	{47, 48, nil, []string{"idx_events_moved_from_id"}},
	{48, 49, nil, []string{"idx_events_cron_run_uid"}},
	{49, 50, []string{"mcp_event_subscriptions"}, []string{"idx_mcp_event_subscriptions_due", "idx_mcp_event_subscriptions_principal_callback", "idx_mcp_event_subscriptions_ended"}},
}

// UpgradesInPlace reports whether in-place steps reach the current schema.
func UpgradesInPlace(version int) bool {
	current := db.CurrentSchemaVersion()
	if version >= current {
		return false
	}
	for _, step := range inPlaceUpgrades {
		if step.From == version {
			if step.To <= version || step.To > current {
				return false
			}
			version = step.To
		}
	}
	return version == current
}

// UpgradeInPlace adds canonical tables and indexes and stamps their version
// atomically.
// The caller must hold the source's cutover lock.
func UpgradeInPlace(ctx context.Context, path string) error {
	sdb, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=rw&_txlock=immediate&_pragma=busy_timeout(%d)", sqliteURIPath(path), sqliteBusyTimeout.Milliseconds()))
	if err != nil {
		return fmt.Errorf("open index upgrade: %w", err)
	}
	defer func() { _ = sdb.Close() }()
	s := &Store{DB: sdb}
	return s.RetryTransient(ctx, func() error {
		tx, err := sdb.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin index upgrade: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
			return fmt.Errorf("read index upgrade version: %w", err)
		}
		if version == db.CurrentSchemaVersion() {
			return nil
		}
		if !UpgradesInPlace(version) {
			return fmt.Errorf("no in-place upgrade path from schema_version %d to %d", version, db.CurrentSchemaVersion())
		}
		tables, indexes, err := canonicalSQL(ctx)
		if err != nil {
			return err
		}
		for _, step := range inPlaceUpgrades {
			if step.From < version {
				continue
			}
			for _, name := range step.Tables {
				ddl, ok := tables[name]
				if !ok {
					return fmt.Errorf("canonical table %s is missing", name)
				}
				if _, err := tx.ExecContext(ctx, ddl); err != nil {
					return fmt.Errorf("add table %s: %w", name, err)
				}
			}
			for _, name := range step.Indexes {
				ddl, ok := indexes[name]
				if !ok {
					return fmt.Errorf("canonical index %s is missing", name)
				}
				if _, err := tx.ExecContext(ctx, ddl); err != nil {
					return fmt.Errorf("add index %s: %w", name, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key='schema_version'`, db.CurrentSchemaVersion()); err != nil {
			return fmt.Errorf("stamp index upgrade: %w", err)
		}
		return tx.Commit()
	})
}

// canonicalSQL reads the canonical schema's table and index DDL by name.
func canonicalSQL(ctx context.Context) (tables, indexes map[string]string, err error) {
	canonical, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, fmt.Errorf("open canonical schema: %w", err)
	}
	defer func() { _ = canonical.Close() }()
	canonical.SetMaxOpenConns(1)
	if _, err := canonical.ExecContext(ctx, schemaSQL); err != nil {
		return nil, nil, fmt.Errorf("create canonical schema: %w", err)
	}
	rows, err := canonical.QueryContext(ctx, `SELECT type,name,sql FROM sqlite_master WHERE type IN ('table','index') AND sql IS NOT NULL`)
	if err != nil {
		return nil, nil, fmt.Errorf("read canonical schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tables, indexes = map[string]string{}, map[string]string{}
	for rows.Next() {
		var kind, name, statement string
		if err := rows.Scan(&kind, &name, &statement); err != nil {
			return nil, nil, fmt.Errorf("read canonical schema entry: %w", err)
		}
		if kind == "table" {
			tables[name] = statement
		} else {
			indexes[name] = statement
		}
	}
	return tables, indexes, rows.Err()
}
