package client

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/config"
)

// StorageInspector reads existing kata storage without bootstrap, migration,
// or cutover. The client consults it before it starts or replaces a local
// daemon. Implementations live outside this package so programs that only
// discover daemons do not link the storage engines.
type StorageInspector interface {
	// ReadProfileIdentity returns a local profile's stored instance UID and
	// schema version. It may return the UID alongside an error when only the
	// schema read failed.
	ReadProfileIdentity(ctx context.Context, profile config.LocalProfileConfig) (instanceUID string, schemaVersion int, err error)
	// PeekCurrentSchema returns the schema version of the current home's
	// storage, or zero when a SQLite database does not exist yet. validateOnly
	// reports PostgreSQL storage in validate mode, which a daemon cannot
	// migrate.
	PeekCurrentSchema(ctx context.Context) (version int, validateOnly bool, err error)
	// CurrentSchemaVersion is the schema version a daemon started by this
	// program serves.
	CurrentSchemaVersion() int
}

var storageInspector StorageInspector

// UseStorageInspector installs the storage reader that starting or replacing
// a local daemon requires. Call it during program initialization, before any
// client resolution. Without it, those operations fail with
// ErrStorageInspectorUnavailable before any process is stopped or started;
// discovery is unaffected.
func UseStorageInspector(inspector StorageInspector) {
	storageInspector = inspector
}

// ErrStorageInspectorUnavailable means this program cannot read kata storage,
// so it cannot check whether a daemon it starts could open that storage.
var ErrStorageInspectorUnavailable = errors.New("this program cannot read kata storage")

func installedStorageInspector() (StorageInspector, error) {
	if storageInspector == nil {
		return nil, ErrStorageInspectorUnavailable
	}
	return storageInspector, nil
}
