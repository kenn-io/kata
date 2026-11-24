package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// inPlaceUpgrades drop retired indexes, then add columns, canonical tables, and
// indexes. A table is added only when it starts empty, so no existing row
// needs converting. An index that a later step drops is no longer canonical,
// so an upgrade that runs both steps never creates it.
var inPlaceUpgrades = []struct {
	From, To    int
	Tables      []string
	Indexes     []string
	DropIndexes []string
}{
	{41, 42, nil, []string{"idx_issues_project_created", "idx_issues_created"}, nil},
	{42, 43, nil, []string{"idx_events_resolved_issue_uid"}, nil},
	{43, 44, nil, []string{"idx_events_reset_from"}, nil},
	{44, 45, nil, []string{"idx_issues_uid_prefix", "idx_issues_project_uid_prefix"}, nil},
	{45, 46, nil, []string{"idx_issues_project_ready_priority", "idx_issues_ready_priority"}, nil},
	{46, 47, nil, []string{"idx_issues_ready_updated"}, nil},
	{47, 48, nil, []string{"idx_events_moved_from_id"}, nil},
	{48, 49, nil, []string{"idx_events_cron_run_uid"}, nil},
	{49, 50, []string{"mcp_event_subscriptions"}, []string{"idx_mcp_event_subscriptions_due", "idx_mcp_event_subscriptions_principal_callback", "idx_mcp_event_subscriptions_ended"}, nil},
	{50, 51, nil, []string{"idx_events_issue_uid_id"}, []string{"idx_events_resolved_issue_uid"}},
	{51, 52, nil, nil, nil},
	{52, 53, nil, []string{"idx_events_project_moves"}, nil},
	{53, 54, nil, nil, nil},
	{54, 55, nil, nil, nil},
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
		// retired holds every index any remaining step drops, regardless of
		// order. This assumes no step re-adds a name an earlier step dropped;
		// such a step would be skipped here and the index never created.
		retired := map[string]bool{}
		for _, step := range inPlaceUpgrades {
			if step.From >= version {
				for _, name := range step.DropIndexes {
					retired[name] = true
				}
			}
		}
		skipped := map[string]bool{}
		for _, step := range inPlaceUpgrades {
			if step.From < version {
				continue
			}
			for _, name := range step.DropIndexes {
				if skipped[name] {
					continue
				}
				if _, err := tx.ExecContext(ctx, `DROP INDEX `+name); err != nil {
					return fmt.Errorf("drop index %s: %w", name, err)
				}
			}
			if step.From == 54 {
				if _, err := tx.ExecContext(ctx, `ALTER TABLE federation_bindings ADD COLUMN retained_through_event_id INTEGER NOT NULL DEFAULT 0`); err != nil {
					return fmt.Errorf("add retained federation history boundary: %w", err)
				}
				match := `SELECT 1 FROM federation_quarantine q JOIN events e ON e.project_id=q.project_id AND e.id BETWEEN q.first_event_id AND q.last_event_id
					WHERE q.project_id=b.project_id AND q.direction='push' AND q.skipped_at IS NULL
					AND e.actor<>b.bound_actor AND ` + db.LocalInstanceOriginSQL("e.origin_instance_uid", "(SELECT value FROM meta WHERE key='instance_uid')") + `
					AND q.error LIKE '%event ' || e.uid || ' actor % does not match bound actor%'`
				eligible := `b.role='spoke' AND b.relay_config IS NULL AND EXISTS (` + match + `)`
				localOrigin := db.LocalInstanceOriginSQL("e.origin_instance_uid", "(SELECT value FROM meta WHERE key='instance_uid')")
				// The SQL fragments use only fixed literals.
				if _, err := tx.ExecContext(ctx, `UPDATE federation_bindings AS b SET retained_through_event_id=(SELECT COALESCE(MAX(id),0) FROM events e WHERE e.project_id=b.project_id AND `+localOrigin+`) WHERE b.role='spoke' AND b.relay_config IS NULL AND EXISTS (SELECT 1 FROM events e WHERE e.project_id=b.project_id AND e.actor<>b.bound_actor AND (e.type LIKE 'issue.%' OR e.type LIKE 'cron.%' OR e.type='project.metadata_updated') AND `+localOrigin+`)`); err != nil { // #nosec G202 -- fixed SQL fragments
					return fmt.Errorf("backfill retained federation history: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `UPDATE federation_quarantine SET skipped_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'), skipped_by='system', skip_reason='retry: retained history upgrade' WHERE direction='push' AND skipped_at IS NULL AND EXISTS (SELECT 1 FROM federation_bindings b WHERE b.project_id=federation_quarantine.project_id AND `+eligible+`)`); err != nil { // #nosec G202 -- fixed SQL fragments
					return fmt.Errorf("retry retained federation history quarantine: %w", err)
				}
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
				if retired[name] {
					skipped[name] = true
					continue
				}
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
