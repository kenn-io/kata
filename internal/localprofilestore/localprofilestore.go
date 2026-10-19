// Package localprofilestore reads a selected local profile's existing storage
// without bootstrap, migration, or cutover. It is separate from
// internal/client so client-only programs that discover a daemon do not link
// the storage engines. The kata binary installs Storage with
// client.UseLocalProfileStorage before it starts local profiles.
package localprofilestore

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/storeopen"
)

// Storage reads local profile storage with this binary's storage engines.
type Storage struct{}

// ReadIdentity reads the instance UID and schema version recorded in the
// profile's existing storage. A schema read failure still reports the UID.
func (Storage) ReadIdentity(ctx context.Context, profile config.LocalProfileConfig) (string, int, error) {
	if isPostgres(profile.DSN) {
		pg := profile.Config.Storage.Postgres
		return pgstore.InspectMetadataWithConfig(ctx, profile.DSN, pgstore.ConfigFromValues(pg.Schema, pg.Mode, pg.SchemaOwner, pg.AllowInsecure))
	}
	store, err := OpenReadOnly(ctx, profile)
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

// CurrentSchemaVersion is the schema version this binary's daemon serves.
func (Storage) CurrentSchemaVersion() int { return db.CurrentSchemaVersion() }

// OpenReadOnly opens an existing selected target without preparation.
func OpenReadOnly(ctx context.Context, profile config.LocalProfileConfig) (db.Storage, error) {
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
