package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// OpenReplacementTarget opens an existing current-schema target without
// bootstrap, cutover, identity seeding or persistent PRAGMA changes. The caller
// must replay an already-prepared snapshot; its metadata replaces the old file's
// metadata. Ownership is checked by ImportReplay under the writer reservation.
func OpenReplacementTarget(ctx context.Context, path string) (*Store, error) {
	sdb, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=rw&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", sqliteURIPath(path)))
	if err != nil {
		return nil, err
	}
	d := &Store{DB: sdb, path: path, replaceTargetMetadata: true, idempotencyLocks: newIdempotencyLockSet()}
	d.readQ = sdb
	version, err := d.currentVersion(ctx)
	if err == nil && version != db.CurrentSchemaVersion() {
		err = fmt.Errorf("replacement requires current schema %d; target has %d; upgrade the target separately before restoring", db.CurrentSchemaVersion(), version)
	}
	if err == nil {
		err = d.RefreshInstanceUID(ctx)
	}
	if err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return d, nil
}
