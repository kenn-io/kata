package pgstore

import (
	_ "embed"
	"fmt"
)

const baselineSchemaVersion = 23

//go:embed migrations/0023_baseline.sql
var baselineSchemaSQL string

//go:embed migrations/0024_postgres_runtime.sql
var postgresRuntimeSchemaSQL string

//go:embed migrations/0025_federation_binding_authority.sql
var federationBindingAuthoritySchemaSQL string

// Migration is one immutable Postgres schema transition. Assets form an exact
// version chain; callers applying them externally must stamp ToVersion only
// after SQL succeeds in the same transaction.
type Migration struct {
	FromVersion int
	ToVersion   int
	Name        string
	SQL         string
}

var migrationAssets = []Migration{
	{
		FromVersion: 0,
		ToVersion:   baselineSchemaVersion,
		Name:        "0023_baseline.sql",
		SQL:         baselineSchemaSQL,
	},
	{
		FromVersion: baselineSchemaVersion,
		ToVersion:   24,
		Name:        "0024_postgres_runtime.sql",
		SQL:         postgresRuntimeSchemaSQL,
	},
	{
		FromVersion: 24,
		ToVersion:   25,
		Name:        "0025_federation_binding_authority.sql",
		SQL:         federationBindingAuthoritySchemaSQL,
	},
}

// Migrations returns the ordered Postgres migration assets. The returned
// slice is detached from the package registry.
func Migrations() []Migration {
	return append([]Migration(nil), migrationAssets...)
}

func migrationPath(fromVersion, toVersion int) ([]Migration, error) {
	if fromVersion == toVersion {
		return nil, nil
	}
	current := fromVersion
	path := make([]Migration, 0, len(migrationAssets))
	for current < toVersion {
		found := false
		for _, migration := range migrationAssets {
			if migration.FromVersion != current {
				continue
			}
			if migration.ToVersion > toVersion || migration.ToVersion <= current {
				return nil, fmt.Errorf("invalid postgres migration %s: %d to %d", migration.Name, migration.FromVersion, migration.ToVersion)
			}
			path = append(path, migration)
			current = migration.ToVersion
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("no postgres migration path from schema_version %d to %d", current, toVersion)
		}
	}
	return path, nil
}
