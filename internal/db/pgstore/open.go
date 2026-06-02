package pgstore

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

//go:embed schema.sql
var schemaSQL string

// Default connection pool sizing. Conservative for v1 single-daemon
// deployments. Future phases may expose these through DSN params or
// [storage] config.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxIdleTime = 5 * time.Minute
)

// Open opens a PG connection pool against dsn using pgx's database/sql
// wrapper. Per-connection runtime params (application_name, statement_timeout,
// idle_in_transaction_session_timeout, and — when db.ReadOnly() is set —
// default_transaction_read_only) ride on the pgx config's RuntimeParams so
// every pooled connection inherits them via the startup packet, rather than
// a one-shot SET that only touches the connection that ran it.
//
// On a writable handle, Open bootstraps the canonical schema in a single
// transaction when the DB has no meta table, then seeds meta.instance_uid.
// Existing DBs at the binary's schema version are left untouched; older or
// newer DBs surface a credential-free error. Read-only handles skip both
// bootstrap and ensureInstanceUID and just open the pool.
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (*Store, error) {
	cfg := db.ApplyOpenOptions(opts...)
	return openInternal(ctx, dsn, cfg.ReadOnly)
}

// openInternal is the shared body shared between Open (option-driven) and
// PeekSchemaVersion (always read-only). Splitting it keeps the option-handling
// surface in one place and saves PeekSchemaVersion from a synthetic options
// slice.
func openInternal(ctx context.Context, dsn string, readOnly bool) (*Store, error) {
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		// pgx.ParseConfig errors can echo DSN fragments — a quoted bad
		// "password=..." kv or an unparseable URL whose path carries
		// credentials. Drop err.Error() entirely and surface only the
		// credential-free canonical form so logs, stderr, and any
		// service journal stay clean. RedactDSN falls back to "" on
		// shapes too ambiguous to safely redact (e.g. an unescaped ':'
		// or '@' in the password); a static placeholder takes over so
		// the error still names what was attempted.
		_ = err
		redacted := config.RedactDSN(dsn)
		if redacted == "" {
			redacted = "<dsn redacted>"
		}
		return nil, fmt.Errorf("parse pgx config for %s", redacted)
	}
	if connConfig.RuntimeParams == nil {
		connConfig.RuntimeParams = map[string]string{}
	}
	// RuntimeParams ship to every new connection via the startup packet, so
	// these GUCs are guaranteed on every pooled connection — not just the
	// one that handled an out-of-band SET ExecContext.
	connConfig.RuntimeParams["application_name"] = "kata"
	connConfig.RuntimeParams["statement_timeout"] = "30s"
	connConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60s"
	if readOnly {
		// Pool-wide read-only enforcement: any transaction opened from
		// any pooled connection starts read-only. Without this on
		// RuntimeParams a one-shot SET on a single connection would
		// leave the rest of the pool able to write.
		connConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}

	connector := stdlib.GetConnector(*connConfig)
	sdb := sql.OpenDB(connector)
	sdb.SetMaxOpenConns(defaultMaxOpenConns)
	sdb.SetMaxIdleConns(defaultMaxIdleConns)
	sdb.SetConnMaxIdleTime(defaultConnMaxIdleTime)
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping pgx: %w", err)
	}
	s := &Store{DB: sdb, dsn: dsn, readOnly: readOnly}
	if readOnly {
		if err := s.cacheInstanceUIDIfPresent(ctx); err != nil {
			_ = sdb.Close()
			return nil, err
		}
		return s, nil
	}
	if err := s.bootstrap(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if err := s.ensureInstanceUID(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	// EnsureSystemProject is a Phase 4 stub; the Phase 3 schema is bootstrapped
	// without seeding the system project so the meta-only acceptance suite can
	// still drive Open against a fresh container.
	return s, nil
}

// bootstrap initializes a fresh database from schema.sql or refuses to open
// a database whose schema_version disagrees with the binary's. Postgres has
// no JSONL-cutover path; the operator must restore a current-schema backup
// before reopening.
func (s *Store) bootstrap(ctx context.Context) error {
	current, err := s.currentVersion(ctx)
	if err != nil {
		return err
	}
	currentBinary := db.CurrentSchemaVersion()
	if current > currentBinary {
		return fmt.Errorf("postgres schema_version %d is newer than binary schema %d",
			current, currentBinary)
	}
	if current > 0 && current < currentBinary {
		return fmt.Errorf("postgres schema_version %d is older than binary schema %d; restore from operator backup before reopening",
			current, currentBinary)
	}
	if current == currentBinary {
		return nil
	}
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema bootstrap: %w", err)
	}
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('schema_version', $1)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		strconv.Itoa(currentBinary)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record schema version %d: %w", currentBinary, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema bootstrap: %w", err)
	}
	return nil
}

// ensureInstanceUID is the single ownership rule for meta.instance_uid: if
// the row is absent it is inserted with a fresh ULID; if present it is read
// into s.instanceUID. Idempotent across reboots and every Open caller.
func (s *Store) ensureInstanceUID(ctx context.Context) error {
	var existing string
	err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&existing)
	if err == nil {
		s.instanceUID = existing
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	fresh, err := katauid.New()
	if err != nil {
		return fmt.Errorf("generate instance_uid: %w", err)
	}
	if _, err := s.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('instance_uid', $1)
		 ON CONFLICT (key) DO NOTHING`, fresh); err != nil {
		return fmt.Errorf("seed instance_uid: %w", err)
	}
	var stored string
	if err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored); err != nil {
		return fmt.Errorf("read instance_uid after seed: %w", err)
	}
	s.instanceUID = stored
	return nil
}
