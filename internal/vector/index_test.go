package vector

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenCreatesAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ix, err = Open(ctx, path) // reopen must not error or reset
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = ix.Close() }()
	var v string
	if err := ix.db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = 'mirror_schema_version'`).Scan(&v); err != nil {
		t.Fatalf("meta: %v", err)
	}
	if v != mirrorSchemaVersion {
		t.Fatalf("version = %q, want %q", v, mirrorSchemaVersion)
	}
}

func TestOpenResetsOnVersionMismatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := ix.db.ExecContext(ctx,
		`UPDATE vector_meta SET value = '0' WHERE key = 'mirror_schema_version'`); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if _, err := ix.db.ExecContext(ctx,
		`INSERT INTO issue_mirror (issue_uid, project_uid, content, content_revision)
		 VALUES ('u1', 'p1', 'c', 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = ix.Close()

	ix, err = Open(ctx, path) // mismatch: sidecar is disposable, rebuild
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = ix.Close() }()
	var n int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM issue_mirror`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("mirror rows after reset = %d, want 0", n)
	}
}
