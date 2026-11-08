package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

var indexOnlyUpgrades = []struct {
	From, To int
	Indexes  []string
}{
	{41, 42, []string{"idx_issues_project_created", "idx_issues_created"}},
	{42, 43, []string{"idx_events_resolved_issue_uid"}},
	{43, 44, []string{"idx_events_reset_from"}},
	{44, 45, []string{"idx_issues_uid_prefix", "idx_issues_project_uid_prefix"}},
	{45, 46, []string{"idx_issues_project_ready_priority", "idx_issues_ready_priority"}},
	{46, 47, []string{"idx_issues_ready_updated"}},
	{47, 48, []string{"idx_events_moved_from_id"}},
	{48, 49, []string{"idx_events_cron_run_uid"}},
}

// UpgradesInPlace reports whether index-only steps reach the current schema.
func UpgradesInPlace(version int) bool {
	current := db.CurrentSchemaVersion()
	if version >= current {
		return false
	}
	for _, step := range indexOnlyUpgrades {
		if step.From == version {
			if step.To <= version || step.To > current {
				return false
			}
			version = step.To
		}
	}
	return version == current
}

// UpgradeInPlace adds canonical indexes and stamps their version atomically.
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
			return fmt.Errorf("no index-only upgrade path from schema_version %d to %d", version, db.CurrentSchemaVersion())
		}
		indexes, err := canonicalIndexSQL(ctx)
		if err != nil {
			return err
		}
		for _, step := range indexOnlyUpgrades {
			if step.From < version {
				continue
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

func canonicalIndexSQL(ctx context.Context) (map[string]string, error) {
	canonical, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open canonical index schema: %w", err)
	}
	defer func() { _ = canonical.Close() }()
	canonical.SetMaxOpenConns(1)
	if _, err := canonical.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("create canonical index schema: %w", err)
	}
	rows, err := canonical.QueryContext(ctx, `SELECT name,sql FROM sqlite_master WHERE type='index' AND sql IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("read canonical indexes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	indexes := make(map[string]string)
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, fmt.Errorf("read canonical index: %w", err)
		}
		indexes[name] = ddl
	}
	return indexes, rows.Err()
}
