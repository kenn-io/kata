package pgstore

import (
	"context"
	"fmt"
	"strconv"
)

// InspectMetadataWithConfig reads only identity/version metadata from an
// existing schema. Like PeekSchemaVersionWithConfig, it bypasses projection
// validation and every schema preparation path, with a read-only pool.
func InspectMetadataWithConfig(ctx context.Context, dsn string, pgConfig Config) (instanceUID string, schemaVersion int, err error) {
	pgConfig.SchemaMode = SchemaModeValidate
	if err := pgConfig.Validate(); err != nil {
		return "", 0, err
	}
	store, err := openInternal(ctx, dsn, pgConfig, true, false, true)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = store.Close() }()
	var version string
	// One statement captures both values in the same read snapshot.
	err = store.QueryRowContext(ctx, `SELECT
		(SELECT value FROM meta WHERE key='instance_uid'),
		(SELECT value FROM meta WHERE key='schema_version')`).Scan(&instanceUID, &version)
	if err != nil {
		return "", 0, fmt.Errorf("read postgres identity metadata: %w", err)
	}
	schemaVersion, err = strconv.Atoi(version)
	if err != nil {
		return instanceUID, 0, fmt.Errorf("postgres schema version is unreadable")
	}
	return instanceUID, schemaVersion, nil
}
