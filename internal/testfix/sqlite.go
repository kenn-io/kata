package testfix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

var sqliteTemplate struct {
	once sync.Once
	data []byte
	err  error
}

// OpenSQLite opens an isolated copy of a schema-ready SQLite template, or
// reopens path unchanged if it exists. Bootstrap, cutover, and existing-file
// tests should keep calling sqlitestore.Open directly.
func OpenSQLite(ctx context.Context, path string) (*sqlitestore.Store, error) {
	sqliteTemplate.once.Do(func() {
		// The process-wide template must survive its first caller's cancellation.
		sqliteTemplate.data, sqliteTemplate.err = buildSQLiteTemplate(context.WithoutCancel(ctx))
	})
	if sqliteTemplate.err != nil {
		return nil, fmt.Errorf("build SQLite test template: %w", sqliteTemplate.err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // test fixture mirrors SQLite's default file mode
	if err == nil {
		_, writeErr := file.Write(sqliteTemplate.data)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return nil, fmt.Errorf("write SQLite test template: %w", err)
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create SQLite test database: %w", err)
	}
	return sqlitestore.Open(ctx, path)
}

func buildSQLiteTemplate(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "kata-sqlite-template-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "kata.db")
	store, err := sqlitestore.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	if _, err := store.ExecContext(ctx, `DELETE FROM meta WHERE key IN ('instance_uid', ?)`, db.MetaKeyInstanceCreatedAt); err != nil {
		return nil, err
	}
	// Reopening a retained system project would consume another AUTOINCREMENT ID.
	if _, err := store.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, db.SystemProjectName); err != nil {
		return nil, err
	}
	if _, err := store.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = 'projects'`); err != nil {
		return nil, err
	}
	if err := store.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path) //nolint:gosec // template path is inside our temporary directory
}
