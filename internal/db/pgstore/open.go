package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"go.kenn.io/kata/internal/db"
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
// Pings the pool to verify connectivity before returning. Migrate must be
// called explicitly to apply schema changes.
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
		return nil, fmt.Errorf("parse pgx config: %w", err)
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
	if err := s.cacheInstanceUIDIfPresent(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return s, nil
}
