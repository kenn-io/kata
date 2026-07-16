package pgstore

import (
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var canonicalSchemaSQL string

// Migration is one immutable Postgres schema transition. Assets form an exact
// version chain; callers applying them externally must stamp ToVersion only
// after SQL succeeds in the same transaction.
type Migration struct {
	FromVersion int
	ToVersion   int
	Name        string
	SQL         string
}

// migrationAssets begins at the first schema version released with Postgres
// support. The first release installs canonicalSchemaSQL directly, so there
// is no historical Postgres database to upgrade in this branch.
var migrationAssets = []Migration{}

// Migrations returns forward migrations from previously released Postgres
// schema versions. The returned slice is detached from the package registry.
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
