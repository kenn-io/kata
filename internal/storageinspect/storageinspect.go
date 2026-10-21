// Package storageinspect reads existing kata storage without bootstrap,
// migration, or cutover, for the checks the client makes before it starts or
// replaces a local daemon. It is separate from internal/client so client-only
// programs that discover a daemon do not link the storage engines. The kata
// binary installs Inspector with client.UseStorageInspector.
package storageinspect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/storeopen"
)

// Inspector reads storage with this binary's storage engines.
type Inspector struct{}

// ReadProfileIdentity reads the instance UID and schema version recorded in
// the profile's existing storage. A schema read failure still reports the UID.
func (Inspector) ReadProfileIdentity(ctx context.Context, profile config.LocalProfileConfig) (string, int, error) {
	if isPostgres(profile.DSN) {
		pg := profile.Config.Storage.Postgres
		return pgstore.InspectMetadataWithConfig(ctx, profile.DSN, pgstore.ConfigFromValues(pg.Schema, pg.Mode, pg.SchemaOwner, pg.AllowInsecure))
	}
	store, err := OpenProfileReadOnly(ctx, profile)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = store.Close() }()
	if err = store.RefreshInstanceUID(ctx); err != nil {
		return "", 0, fmt.Errorf("read instance identity: %w", err)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		return store.InstanceUID(), 0, fmt.Errorf("read schema version: %w", err)
	}
	return store.InstanceUID(), version, nil
}

// PeekCurrentSchema reads the schema version of the storage a daemon for the
// current home would open, using the same storage settings that daemon reads.
// A SQLite database that does not exist yet reports zero.
func (Inspector) PeekCurrentSchema(ctx context.Context) (int, bool, error) {
	dsn, err := config.KataDSN(ctx)
	if err != nil {
		return 0, false, err
	}
	backend, err := storeopen.BackendForDSN(dsn)
	if err != nil {
		return 0, false, err
	}
	validateOnly := false
	switch backend {
	case storeopen.BackendSQLite:
		if _, err := os.Stat(strings.TrimPrefix(dsn, "sqlite://")); errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
	case storeopen.BackendPostgres:
		pg, err := config.KataPostgresStorageConfig(ctx)
		if err != nil {
			return 0, false, err
		}
		mode := pgstore.ConfigFromValues(pg.Schema, pg.Mode, pg.SchemaOwner, pg.AllowInsecure).SchemaMode
		validateOnly = mode == pgstore.SchemaModeValidate
	}
	version, err := storeopen.PeekSchemaVersion(ctx, dsn)
	return version, validateOnly, err
}

// CurrentSchemaVersion is the schema version this binary's daemon serves.
func (Inspector) CurrentSchemaVersion() int { return db.CurrentSchemaVersion() }

// OpenProfileReadOnly opens a selected profile's existing storage without
// preparation.
func OpenProfileReadOnly(ctx context.Context, profile config.LocalProfileConfig) (db.Storage, error) {
	if !isPostgres(profile.DSN) {
		return storeopen.OpenReadOnly(ctx, profile.DSN)
	}
	pg := profile.Config.Storage.Postgres
	// OpenWithConfig plus ReadOnly preserves the selected schema and cannot
	// bootstrap it. OpenReadOnly's default PostgreSQL dispatch uses current-home
	// configuration, so it is deliberately not used for a selected PG profile.
	return storeopen.OpenWithConfig(ctx, profile.DSN, storeopen.Config{Postgres: pgstore.ConfigFromValues(pg.Schema, pg.Mode, pg.SchemaOwner, pg.AllowInsecure)}, db.ReadOnly())
}

func isPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}
