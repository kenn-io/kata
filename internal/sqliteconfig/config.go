// Package sqliteconfig contains SQLite setup shared by kata's canonical and
// derived-state databases.
package sqliteconfig

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const testFastSQLiteEnv = "KATA_TEST_FAST_SQLITE"

// FastTestMode reports whether a Go test binary explicitly requested
// reduced-durability SQLite settings. Production binaries ignore the
// environment variable so an exported test setting cannot weaken real data.
func FastTestMode() bool {
	if os.Getenv(testFastSQLiteEnv) != "1" {
		return false
	}
	bin := strings.ToLower(filepath.Base(os.Args[0]))
	return strings.HasSuffix(bin, ".test") || strings.HasSuffix(bin, ".test.exe")
}

// ConfigureWAL enables WAL only after reduced-durability test settings have
// taken effect on the same connection. Drivers also receive synchronous=OFF
// in their DSNs so later pooled connections retain the durability setting.
func ConfigureWAL(ctx context.Context, db *sql.DB, fast bool) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if fast {
		if _, err := conn.ExecContext(ctx, "PRAGMA synchronous=OFF"); err != nil {
			return fmt.Errorf("disable synchronous writes: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA temp_store=MEMORY"); err != nil {
			return fmt.Errorf("configure temporary storage: %w", err)
		}
	}

	var mode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode(WAL)").Scan(&mode); err != nil {
		return fmt.Errorf("enable WAL: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("enable WAL: SQLite selected journal mode %q", mode)
	}
	return nil
}
