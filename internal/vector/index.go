// Package vector manages kata's derived semantic-search state: a sidecar
// SQLite database holding a mirror of embeddable issue content plus kit's
// sqlitevec vector store (generation bookkeeping and per-generation vec0 KNN
// tables). Everything in the sidecar is rebuildable from kata.db: on any
// structural mismatch the file is deleted and rebuilt, never migrated.
package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// mirrorSchemaVersion guards the kata-owned tables in the sidecar. Bump it
// when issue_mirror or vector_meta change shape, or to discard stored vectors
// that a since-fixed bug may have poisoned; mismatch deletes the file and the
// daemon re-embeds from kata.db.
//
// 2: discard vectors written before the embedding client rejected null
// components and zero-norm vectors; stamped-invalid vectors are otherwise
// served forever.
const mirrorSchemaVersion = "2"

const vectorsPrefix = "issue_vectors"

// Index is the open sidecar database plus kit's store bound to it.
type Index struct {
	db        *sql.DB
	store     *sqlitevec.Store[string, string]
	flowStore kitvec.Store[string, string]
	pg        *postgresIndex
	path      string
	ownsDB    bool
}

// Open opens (or creates) the sidecar at path. A mirror schema version
// mismatch deletes and recreates the file: the sidecar is derived state and
// re-embedding is the supported rebuild path.
func Open(ctx context.Context, path string) (*Index, error) {
	db, err := openSidecar(path)
	if err != nil {
		return nil, err
	}
	ok, err := mirrorSchemaCurrent(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !ok {
		_ = db.Close()
		if err := removeSidecar(path); err != nil {
			return nil, err
		}
		if db, err = openSidecar(path); err != nil {
			return nil, err
		}
	}
	if err := ensureMirrorSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	store, err := sqlitevec.New[string, string](ctx, db, sqlitevec.Schema{
		DocsTable:      "issue_mirror",
		IDColumn:       "issue_uid",
		ContentColumn:  "content",
		EmbedGenColumn: "embed_gen",
		VectorsPrefix:  vectorsPrefix,
		RevisionColumn: "content_revision",
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("vector: init kit store: %w", err)
	}
	return &Index{db: db, store: store, flowStore: store, path: path, ownsDB: true}, nil
}

// OpenPostgres binds the semantic index to canonical pgvector tables on an
// already prepared Kata PostgreSQL pool. The caller retains ownership of db.
func OpenPostgres(ctx context.Context, db *sql.DB) (*Index, error) {
	pg := &postgresIndex{db: db}
	if err := pg.validate(ctx); err != nil {
		return nil, err
	}
	return &Index{db: db, flowStore: pg, pg: pg}, nil
}

// Close releases the sidecar handle.
func (ix *Index) Close() error {
	if !ix.ownsDB {
		return nil
	}
	return ix.db.Close()
}

func openSidecar(path string) (*sql.DB, error) {
	sqlitevec.Register() // no-op on modernc builds; required on cgo builds
	db, err := sql.Open(sidecarDriver, sidecarDSN(path))
	if err != nil {
		return nil, fmt.Errorf("vector: open sidecar %s: %w", path, err)
	}
	return db, nil
}

func removeSidecar(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("vector: reset sidecar %s: %w", p, err)
		}
	}
	return nil
}

// mirrorSchemaCurrent reports whether vector_meta exists and records the
// current mirror schema version. A missing table (fresh file) is current.
func mirrorSchemaCurrent(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'vector_meta'`,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("vector: probe sidecar: %w", err)
	}
	if n == 0 {
		return true, nil
	}
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = 'mirror_schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("vector: read mirror schema version: %w", err)
	}
	return v == mirrorSchemaVersion, nil
}

func ensureMirrorSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS vector_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS issue_mirror (
  issue_uid        TEXT PRIMARY KEY,
  project_uid      TEXT NOT NULL,
  content          TEXT NOT NULL,
  content_revision INTEGER NOT NULL,
  embed_gen        TEXT
);
INSERT INTO vector_meta (key, value) VALUES ('mirror_schema_version', '`+mirrorSchemaVersion+`')
ON CONFLICT(key) DO NOTHING;`)
	if err != nil {
		return fmt.Errorf("vector: ensure mirror schema: %w", err)
	}
	return nil
}
