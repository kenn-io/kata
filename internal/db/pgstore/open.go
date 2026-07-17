package pgstore

import (
	"context"
	"crypto/tls"
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
	defaultMaxOpenConns     = 25
	defaultMaxIdleConns     = 5
	idempotencyMaxOpenConns = 4
	idempotencyMaxIdleConns = 1
	defaultConnMaxIdleTime  = 5 * time.Minute
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
	return openInternal(ctx, dsn, pgConfig, openConfig.ReadOnly, openConfig.Serving, false)
}

// openInternal is the shared body between option-driven opens and the
// lifecycle-bypassing schema-version probe.
func openInternal(
	ctx context.Context,
	dsn string,
	pgConfig Config,
	readOnly bool,
	serving bool,
	bypassLifecycle bool,
) (*Store, error) {
	if !bypassLifecycle && pgConfig.SchemaMode == SchemaModeValidate && pgConfig.SchemaOwner == "" {
		return nil, fmt.Errorf("postgres schema owner is required in validation mode")
	}
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

	sdb := newPostgresPool(*connConfig, defaultMaxOpenConns, defaultMaxIdleConns)
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping pgx: %w", err)
	}
	var currentUser string
	if err := sdb.QueryRowContext(ctx, `SELECT current_user`).Scan(&currentUser); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("resolve authenticated postgres role: %w", err)
	}
	schemaOwner := pgConfig.SchemaOwner
	if schemaOwner == "" {
		schemaOwner = currentUser
	}
	if pgConfig.SchemaMode == SchemaModeBootstrap && schemaOwner != currentUser {
		_ = sdb.Close()
		return nil, fmt.Errorf(
			"postgres bootstrap requires authenticated role %q to match configured schema owner %q",
			currentUser, schemaOwner,
		)
	}
	s := &Store{
		DB: sdb, dsn: dsn, schema: pgConfig.Schema, schemaOwner: schemaOwner, readOnly: readOnly,
		federationLease: &federationRunnerLeaseState{},
	}
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
	installedFresh, err := s.prepareSchema(ctx, pgConfig.SchemaMode)
	if err != nil {
		_ = sdb.Close()
		return nil, err
	}
	s.installedFreshSchema = installedFresh
	s.idempotencyDB = newPostgresPool(*connConfig, idempotencyMaxOpenConns, idempotencyMaxIdleConns)
	if err := s.idempotencyDB.PingContext(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("ping postgres idempotency coordinator: %w", err)
	}
	if serving {
		if err := s.acquireServingLease(ctx); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	if err := s.ensureInstanceUID(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.EnsureSystemProject(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func newPostgresPool(connConfig pgx.ConnConfig, maxOpen, maxIdle int) *sql.DB {
	pool := sql.OpenDB(stdlib.GetConnector(connConfig))
	pool.SetMaxOpenConns(maxOpen)
	pool.SetMaxIdleConns(maxIdle)
	pool.SetConnMaxIdleTime(defaultConnMaxIdleTime)
	return pool
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
		host:        connConfig.Host,
		verifiedTLS: tlsVerifiesHost(connConfig.Host, connConfig.TLSConfig),
	})
	for _, fallback := range connConfig.Fallbacks {
		candidates = append(candidates, candidate{
			host:        fallback.Host,
			verifiedTLS: tlsVerifiesHost(fallback.Host, fallback.TLSConfig),
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

func tlsVerifiesHost(host string, cfg *tls.Config) bool {
	return cfg != nil && !cfg.InsecureSkipVerify && cfg.ServerName != "" &&
		strings.EqualFold(strings.TrimSuffix(cfg.ServerName, "."), strings.TrimSuffix(host, "."))
}

func (s *Store) acquireServingLease(ctx context.Context) error {
	conn, err := s.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve postgres serving connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`SELECT pg_advisory_lock_shared(hashtextextended($1, 0))`,
		"kata:pgstore:serving:"+s.schema); err != nil {
		_ = conn.Close()
		return fmt.Errorf("acquire postgres serving lease: %w", mapSQLError(err, nil))
	}
	s.servingConn = conn
	return nil
}

func acquireExclusiveServingLease(ctx context.Context, tx *sql.Tx, schema string) error {
	_, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"kata:pgstore:serving:"+schema)
	return mapSQLError(err, nil)
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

func (s *Store) prepareSchema(ctx context.Context, mode SchemaMode) (bool, error) {
	if mode == SchemaModeValidate {
		return false, s.validateSchema(ctx)
	}
	installedFresh, err := s.bootstrap(ctx)
	if err != nil {
		return false, err
	}
	if err := s.validateSchema(ctx); err != nil {
		return installedFresh, err
	}
	return installedFresh, nil
}

// bootstrap serializes schema installation with a transaction-scoped advisory
// lock. Schema creation, every migration asset, and its version stamp commit
// atomically, so another opener cannot observe a partial installation.
func (s *Store) bootstrap(ctx context.Context) (bool, error) {
	currentBinary := db.CurrentSchemaVersion()
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin schema bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := acquireSchemaMigrationLock(ctx, tx); err != nil {
		return false, fmt.Errorf("lock schema migrations: %w", err)
	}

	var schemaExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, s.schema).Scan(&schemaExists); err != nil {
		return false, fmt.Errorf("inspect postgres schema %q: %w", s.schema, err)
	}
	if !schemaExists {
		if _, err := tx.ExecContext(ctx, `CREATE SCHEMA `+quoteIdentifier(s.schema)); err != nil {
			return false, fmt.Errorf("create postgres schema %q: %w", s.schema, err)
		}
		if _, err := tx.ExecContext(ctx,
			`REVOKE CREATE ON SCHEMA `+quoteIdentifier(s.schema)+` FROM PUBLIC`); err != nil {
			return false, fmt.Errorf("secure postgres schema %q: %w", s.schema, err)
		}
	}
	// Inspect metadata with pg_catalog first. A pre-existing schema is not
	// trusted until its Kata migration metadata has been validated below.
	if _, err := tx.ExecContext(ctx,
		`SET LOCAL search_path TO pg_catalog, `+quoteIdentifier(s.schema)+`, pg_temp`); err != nil {
		return false, fmt.Errorf("select postgres schema %q: %w", s.schema, err)
	}

	if schemaExists {
		trustedMetadata, err := trustedMetadataTableExistsTx(ctx, tx, s.schema)
		if err != nil {
			return false, err
		}
		if !trustedMetadata {
			return false, fmt.Errorf("postgres schema %q exists without migration metadata", s.schema)
		}
	}
	createGrantee, err := untrustedSchemaCreateGrantee(ctx, tx, s.schema)
	if err != nil {
		return false, err
	}
	if createGrantee != "" {
		return false, fmt.Errorf(
			"postgres schema %q grants CREATE privilege to non-owner %q",
			s.schema, createGrantee,
		)
	}
	current, err := currentVersionTx(ctx, tx, s.schema)
	if err != nil {
		return false, err
	}
	if current == 0 && schemaExists {
		return false, fmt.Errorf("postgres schema %q exists without migration metadata", s.schema)
	}
	if current > currentBinary {
		return false, fmt.Errorf("postgres schema_version %d is newer than binary schema %d", current, currentBinary)
	}
	// Unqualified CREATE statements target the first search-path schema. At
	// this point the application schema is trusted: this transaction created
	// it, or it already carries valid Kata migration metadata. This also pins
	// the SECURITY DEFINER adoption helper's captured path with pg_temp last.
	if _, err := tx.ExecContext(ctx,
		`SET LOCAL search_path TO `+quoteIdentifier(s.schema)+`, pg_catalog, pg_temp`); err != nil {
		return false, fmt.Errorf("select trusted postgres schema %q: %w", s.schema, err)
	}

	if current == 0 {
		if _, err := tx.ExecContext(ctx, canonicalSchemaSQL); err != nil {
			return false, fmt.Errorf("install canonical postgres schema: %w", err)
		}
		if err := recordSchemaVersion(ctx, tx, currentBinary, "canonical postgres schema"); err != nil {
			return false, err
		}
	} else {
		migrations, err := migrationPath(current, currentBinary)
		if err != nil {
			return false, err
		}
		for _, migration := range migrations {
			if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
				return false, fmt.Errorf("apply postgres migration %s: %w", migration.Name, err)
			}
			if err := recordSchemaVersion(ctx, tx, migration.ToVersion, migration.Name); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit schema bootstrap: %w", err)
	}
	return !schemaExists, nil
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
	if err := s.validateSchemaOwnership(ctx); err != nil {
		return err
	}
	if err := s.validateRuntimePrivileges(ctx); err != nil {
		return err
	}
	if err := s.validateSchemaManifest(ctx); err != nil {
		return err
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

var canonicalTableNames = map[string]struct{}{
	"api_tokens": {}, "comments": {}, "events": {}, "federation_bindings": {},
	"federation_enrollments": {}, "federation_quarantine": {}, "federation_sync_status": {},
	"import_mappings": {}, "issue_claims": {}, "issue_labels": {}, "issue_sync_bindings": {},
	"issue_sync_status": {}, "issue_vector_chunks": {}, "issue_vector_generations": {},
	"issue_vector_mirror": {}, "issue_vector_stamps": {}, "issues": {}, "issues_search": {},
	"links": {}, "meta": {}, "pending_claim_requests": {}, "project_aliases": {},
	"project_purge_log": {}, "projects": {}, "purge_log": {}, "recurrences": {},
}

func (s *Store) validateSchemaOwnership(ctx context.Context) error {
	var actualOwner string
	err := s.QueryRowContext(ctx, `
		SELECT pg_catalog.pg_get_userbyid(n.nspowner)
		  FROM pg_catalog.pg_namespace n
		 WHERE n.nspname = $1`, s.schema).Scan(&actualOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres schema %q is not installed", s.schema)
	}
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q owner: %w", s.schema, err)
	}
	if actualOwner != s.schemaOwner {
		return fmt.Errorf(
			"postgres schema %q owner %q does not match trusted owner %q",
			s.schema, actualOwner, s.schemaOwner,
		)
	}
	createGrantee, err := untrustedSchemaCreateGrantee(ctx, s, s.schema)
	if err != nil {
		return err
	}
	if createGrantee != "" {
		return fmt.Errorf(
			"postgres schema %q grants CREATE privilege to non-owner %q",
			s.schema, createGrantee,
		)
	}

	rows, err := s.QueryContext(ctx, `
		SELECT c.relname, c.relkind::text, pg_catalog.pg_get_userbyid(c.relowner),
		       c.relrowsecurity, c.relforcerowsecurity
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q relations: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	foundTables := make(map[string]struct{}, len(canonicalTableNames))
	for rows.Next() {
		var name, kind, owner string
		var rowSecurity, forceRowSecurity bool
		if err := rows.Scan(&name, &kind, &owner, &rowSecurity, &forceRowSecurity); err != nil {
			return fmt.Errorf("scan postgres schema %q relation ownership: %w", s.schema, err)
		}
		if owner != s.schemaOwner {
			return fmt.Errorf(
				"postgres schema %q relation %q owner %q does not match trusted owner %q",
				s.schema, name, owner, s.schemaOwner,
			)
		}
		if _, expected := canonicalTableNames[name]; expected {
			if kind != "r" && kind != "p" {
				return fmt.Errorf("postgres schema %q relation %q has unexpected kind %q", s.schema, name, kind)
			}
			if rowSecurity || forceRowSecurity {
				return fmt.Errorf("postgres schema %q relation %q has row security enabled", s.schema, name)
			}
			foundTables[name] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q relations: %w", s.schema, err)
	}
	for name := range canonicalTableNames {
		if _, ok := foundTables[name]; !ok {
			return fmt.Errorf("postgres schema %q is missing canonical relation %q", s.schema, name)
		}
	}

	var untrustedKind, untrustedName, untrustedOwner string
	err = s.QueryRowContext(ctx, `
		SELECT object_kind, object_name, object_owner
		  FROM (
			SELECT 'function'::text AS object_kind, p.proname::text AS object_name,
			       pg_catalog.pg_get_userbyid(p.proowner)::text AS object_owner
			  FROM pg_catalog.pg_proc p
			  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = $1
			UNION ALL
			SELECT 'type', t.typname, pg_catalog.pg_get_userbyid(t.typowner)
			  FROM pg_catalog.pg_type t
			  JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
			 WHERE n.nspname = $1
			UNION ALL
			SELECT 'text search configuration', c.cfgname,
			       pg_catalog.pg_get_userbyid(c.cfgowner)
			  FROM pg_catalog.pg_ts_config c
			  JOIN pg_catalog.pg_namespace n ON n.oid = c.cfgnamespace
			 WHERE n.nspname = $1
			UNION ALL
			SELECT 'operator', o.oprname, pg_catalog.pg_get_userbyid(o.oprowner)
			  FROM pg_catalog.pg_operator o
			  JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace
			 WHERE n.nspname = $1
		  ) owned_objects
		 WHERE object_owner <> $2
		 LIMIT 1`, s.schema, s.schemaOwner).Scan(&untrustedKind, &untrustedName, &untrustedOwner)
	if err == nil {
		return fmt.Errorf(
			"postgres schema %q %s %q owner %q does not match trusted owner %q",
			s.schema, untrustedKind, untrustedName, untrustedOwner, s.schemaOwner,
		)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect postgres schema %q object ownership: %w", s.schema, err)
	}
	return nil
}

type schemaRowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func untrustedSchemaCreateGrantee(ctx context.Context, queryer schemaRowQueryer, schema string) (string, error) {
	var createGrantee string
	err := queryer.QueryRowContext(ctx, `
		SELECT CASE WHEN acl.grantee = 0 THEN 'PUBLIC'
		            ELSE pg_catalog.pg_get_userbyid(acl.grantee) END
		  FROM pg_catalog.pg_namespace n
		 CROSS JOIN LATERAL pg_catalog.aclexplode(
		       COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))
		 ) acl
		 WHERE n.nspname = $1
		   AND acl.privilege_type = 'CREATE'
		   AND acl.grantee <> n.nspowner
		 LIMIT 1`, schema).Scan(&createGrantee)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect postgres schema %q CREATE privileges: %w", schema, err)
	}
	return createGrantee, nil
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
	query := `SELECT value FROM ` + quoteIdentifier(schema) + `.meta WHERE key = 'schema_version'`
	err := tx.QueryRowContext(ctx, query).Scan(&value) // #nosec G202 -- schema is validated and quoted.
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

func trustedMetadataTableExistsTx(ctx context.Context, tx *sql.Tx, schema string) (bool, error) {
	var trusted bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_catalog.pg_namespace n
			  JOIN pg_catalog.pg_class c
			    ON c.relnamespace = n.oid AND c.relname = 'meta'
			 WHERE n.nspname = $1
			   AND n.nspowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
			   AND c.relowner = n.nspowner
			   AND c.relkind IN ('r', 'p')
			   AND NOT c.relrowsecurity
			   AND NOT c.relforcerowsecurity
		)`, schema).Scan(&trusted)
	if err != nil {
		return false, fmt.Errorf("inspect postgres schema %q migration metadata owner: %w", schema, err)
	}
	return trusted, nil
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
