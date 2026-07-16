package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

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
	return OpenWithConfig(ctx, dsn, DefaultConfig(), opts...)
}

// OpenWithConfig opens an exact schema using either standalone bootstrap or
// validation-only startup. Validation occurs before DSN parsing so an invalid
// schema never reaches a connection startup parameter.
func OpenWithConfig(ctx context.Context, dsn string, pgConfig Config, opts ...db.OpenOption) (*Store, error) {
	if err := pgConfig.Validate(); err != nil {
		return nil, err
	}
	openConfig := db.ApplyOpenOptions(opts...)
	return openInternal(ctx, dsn, pgConfig, openConfig.ReadOnly, false)
}

// openInternal is the shared body between option-driven opens and the
// lifecycle-bypassing schema-version probe.
func openInternal(
	ctx context.Context,
	dsn string,
	pgConfig Config,
	readOnly bool,
	bypassLifecycle bool,
) (*Store, error) {
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
	if err := validatePostgresTransport(connConfig, pgConfig.AllowInsecure); err != nil {
		return nil, err
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
	connConfig.RuntimeParams["search_path"] = quoteIdentifier(pgConfig.Schema)
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
	s := &Store{DB: sdb, dsn: dsn, schema: pgConfig.Schema, readOnly: readOnly}
	if bypassLifecycle {
		return s, nil
	}
	if readOnly {
		// A read-only pool cannot bootstrap, but it must still reject missing
		// or version-mismatched schemas before serving reads.
		if err := s.validateSchema(ctx); err != nil {
			_ = sdb.Close()
			return nil, err
		}
		if err := s.cacheInstanceUIDIfPresent(ctx); err != nil {
			_ = sdb.Close()
			return nil, err
		}
		if err := s.validateSystemProject(ctx); err != nil {
			_ = sdb.Close()
			return nil, err
		}
		return s, nil
	}
	if err := s.prepareSchema(ctx, pgConfig.SchemaMode); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if err := s.ensureInstanceUID(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if err := s.EnsureSystemProject(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return s, nil
}

func validatePostgresTransport(connConfig *pgx.ConnConfig, allowInsecure bool) error {
	if allowInsecure {
		return nil
	}
	type candidate struct {
		host        string
		verifiedTLS bool
	}
	candidates := make([]candidate, 0, 1+len(connConfig.Fallbacks))
	candidates = append(candidates, candidate{
		host: connConfig.Host,
		verifiedTLS: connConfig.TLSConfig != nil &&
			!connConfig.TLSConfig.InsecureSkipVerify,
	})
	for _, fallback := range connConfig.Fallbacks {
		candidates = append(candidates, candidate{
			host: fallback.Host,
			verifiedTLS: fallback.TLSConfig != nil &&
				!fallback.TLSConfig.InsecureSkipVerify,
		})
	}
	for _, candidate := range candidates {
		if isLocalPostgresHost(candidate.host) || candidate.verifiedTLS {
			continue
		}
		return fmt.Errorf(
			"remote postgres connections require verified TLS for every connection candidate; use sslmode=verify-full or explicitly allow insecure postgres transport",
		)
	}
	return nil
}

func isLocalPostgresHost(host string) bool {
	if strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Store) validateSystemProject(ctx context.Context) error {
	project, err := s.SystemProject(ctx)
	if err != nil {
		return fmt.Errorf("validate system project: %w", err)
	}
	if project.UID != db.SystemProjectUID {
		return fmt.Errorf("validate system project: %s has uid %q, want %q",
			db.SystemProjectName, project.UID, db.SystemProjectUID)
	}
	return nil
}

func (s *Store) prepareSchema(ctx context.Context, mode SchemaMode) error {
	if mode == SchemaModeValidate {
		return s.validateSchema(ctx)
	}
	return s.bootstrap(ctx)
}

// bootstrap serializes schema installation with a transaction-scoped advisory
// lock. Schema creation, every migration asset, and its version stamp commit
// atomically, so another opener cannot observe a partial installation.
func (s *Store) bootstrap(ctx context.Context) error {
	currentBinary := db.CurrentSchemaVersion()
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := acquireSchemaMigrationLock(ctx, tx); err != nil {
		return fmt.Errorf("lock schema migrations: %w", err)
	}

	var schemaExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, s.schema).Scan(&schemaExists); err != nil {
		return fmt.Errorf("inspect postgres schema %q: %w", s.schema, err)
	}
	if !schemaExists {
		if _, err := tx.ExecContext(ctx, `CREATE SCHEMA `+quoteIdentifier(s.schema)); err != nil {
			return fmt.Errorf("create postgres schema %q: %w", s.schema, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+quoteIdentifier(s.schema)); err != nil {
		return fmt.Errorf("select postgres schema %q: %w", s.schema, err)
	}

	current, err := currentVersionTx(ctx, tx, s.schema)
	if err != nil {
		return err
	}
	if current == 0 && schemaExists {
		var hasTables bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1)`, s.schema).
			Scan(&hasTables); err != nil {
			return fmt.Errorf("inspect postgres schema %q tables: %w", s.schema, err)
		}
		if hasTables {
			return fmt.Errorf("postgres schema %q exists without migration metadata", s.schema)
		}
	}
	if current > currentBinary {
		return fmt.Errorf("postgres schema_version %d is newer than binary schema %d", current, currentBinary)
	}

	if current == 0 {
		if _, err := tx.ExecContext(ctx, canonicalSchemaSQL); err != nil {
			return fmt.Errorf("install canonical postgres schema: %w", err)
		}
		if err := recordSchemaVersion(ctx, tx, currentBinary, "canonical postgres schema"); err != nil {
			return err
		}
	} else {
		migrations, err := migrationPath(current, currentBinary)
		if err != nil {
			return err
		}
		for _, migration := range migrations {
			if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
				return fmt.Errorf("apply postgres migration %s: %w", migration.Name, err)
			}
			if err := recordSchemaVersion(ctx, tx, migration.ToVersion, migration.Name); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema bootstrap: %w", err)
	}
	return nil
}

func recordSchemaVersion(ctx context.Context, tx *sql.Tx, version int, source string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('schema_version', $1)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		strconv.Itoa(version)); err != nil {
		return fmt.Errorf("record postgres schema version for %s: %w", source, err)
	}
	return nil
}

func acquireSchemaMigrationLock(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('kata:pgstore:migrations', 0))`)
	return err
}

func (s *Store) validateSchema(ctx context.Context) error {
	var schemaExists bool
	if err := s.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, s.schema).Scan(&schemaExists); err != nil {
		return fmt.Errorf("inspect postgres schema %q: %w", s.schema, err)
	}
	if !schemaExists {
		return fmt.Errorf("postgres schema %q is not installed", s.schema)
	}
	current, err := s.currentVersion(ctx)
	if err != nil {
		return err
	}
	if current == 0 {
		return fmt.Errorf("postgres schema %q has no migration metadata", s.schema)
	}
	currentBinary := db.CurrentSchemaVersion()
	if current != currentBinary {
		return fmt.Errorf("postgres schema_version %d does not match binary schema %d", current, currentBinary)
	}
	return nil
}

func currentVersionTx(ctx context.Context, tx *sql.Tx, schema string) (int, error) {
	var metaExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = $1 AND table_name = 'meta')`, schema).Scan(&metaExists); err != nil {
		return 0, fmt.Errorf("inspect postgres schema %q metadata: %w", schema, err)
	}
	if !metaExists {
		return 0, nil
	}
	var value string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	version, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse schema_version %q: %w", value, err)
	}
	return version, nil
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
