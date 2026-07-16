package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// RemoveFreshSchema removes the default schema only when it is still the exact
// empty installation identified by expectedInstanceUID. It is the rollback
// path for a failed first JSONL import; changed identity or any domain row
// makes it fail closed.
func RemoveFreshSchema(ctx context.Context, dsn, expectedInstanceUID string) error {
	store, err := openInternal(ctx, dsn, DefaultConfig(), false, true)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	tx, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin fresh schema cleanup: %w", mapSQLError(err, nil))
	}
	defer func() { _ = tx.Rollback() }()
	if err := acquireSchemaMigrationLock(ctx, tx); err != nil {
		return fmt.Errorf("lock fresh schema cleanup: %w", mapSQLError(err, nil))
	}

	tables, err := schemaTableNames(ctx, tx)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return fmt.Errorf("remove fresh postgres schema: no kata tables found")
	}
	quoted := make([]string, len(tables))
	for i, table := range tables {
		quoted[i] = quoteIdentifier(table)
	}
	if _, err := tx.ExecContext(ctx,
		`LOCK TABLE `+strings.Join(quoted, ", ")+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("lock fresh schema tables: %w", mapSQLError(err, nil))
	}
	if err := validateFreshSchema(ctx, tx, tables, expectedInstanceUID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP SCHEMA `+quoteIdentifier(DefaultSchema)+` CASCADE`); err != nil {
		return fmt.Errorf("remove fresh postgres schema: %w", mapSQLError(err, nil))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fresh schema cleanup: %w", mapSQLError(err, nil))
	}
	return nil
}

func schemaTableNames(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT tablename FROM pg_catalog.pg_tables
		 WHERE schemaname = current_schema() ORDER BY tablename`)
	if err != nil {
		return nil, fmt.Errorf("inspect fresh schema tables: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("scan fresh schema table: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspect fresh schema tables: %w", mapSQLError(err, nil))
	}
	return tables, nil
}

func validateFreshSchema(
	ctx context.Context,
	tx *sql.Tx,
	tables []string,
	expectedInstanceUID string,
) error {
	var instanceUID, schemaVersion string
	var metaRows int
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(MAX(value) FILTER (WHERE key='instance_uid'), ''),
			COALESCE(MAX(value) FILTER (WHERE key='schema_version'), '')
		FROM meta`).Scan(&metaRows, &instanceUID, &schemaVersion); err != nil {
		return fmt.Errorf("inspect fresh schema metadata: %w", mapSQLError(err, nil))
	}
	if metaRows != 3 || instanceUID != expectedInstanceUID ||
		schemaVersion != strconv.Itoa(db.CurrentSchemaVersion()) {
		return fmt.Errorf("remove fresh postgres schema: target metadata changed")
	}

	var projects, systemProjects int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE uid=$1 AND name=$2)
		FROM projects`, db.SystemProjectUID, db.SystemProjectName).
		Scan(&projects, &systemProjects); err != nil {
		return fmt.Errorf("inspect fresh schema projects: %w", mapSQLError(err, nil))
	}
	if projects != 1 || systemProjects != 1 {
		return fmt.Errorf("remove fresh postgres schema: target contains project state")
	}
	for _, table := range tables {
		if table == "meta" || table == "projects" {
			continue
		}
		var populated bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM `+quoteIdentifier(table)+` LIMIT 1)`).Scan(&populated); err != nil {
			return fmt.Errorf("inspect fresh schema table %s: %w", table, mapSQLError(err, nil))
		}
		if populated {
			return fmt.Errorf("remove fresh postgres schema: target table %s contains state", table)
		}
	}
	return nil
}
