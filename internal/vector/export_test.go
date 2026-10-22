package vector

import (
	"context"

	kitvec "go.kenn.io/kit/vector"
)

// This file is compiled only into the test binary. It gives the external
// vector_test package access to package internals. It must not import
// internal/testenv or internal/daemon: those import internal/vector, and an
// in-package test file may not close that cycle.

// ErrReconcilerLeaseNotHeldForTest exposes the sentinel every derived-state
// mutator returns when this index holds no reconciler lease.
var ErrReconcilerLeaseNotHeldForTest = errReconcilerLeaseNotHeld

// SaveVectorsForTest reaches the one derived-state mutator that has no
// exported Index method: kit's fill drives it through the flow store.
func (ix *Index) SaveVectorsForTest(
	ctx context.Context,
	gen, doc string,
	revision any,
	vectors []kitvec.ChunkVector,
) error {
	return ix.flowStore.SaveVectors(ctx, gen, doc, revision, vectors)
}

// ClearReconcilerLeaseForTest forgets the recorded lease session without
// releasing its advisory lock or closing its connection, so a test can assert
// that fencing comes from the representation rather than from a terminated
// connection erroring.
func (ix *Index) ClearReconcilerLeaseForTest() {
	conn, err := ix.pg.leaseSession()
	if err != nil {
		return
	}
	ix.pg.releaseLease(conn)
}

// SeedMirrorForTest upserts one mirror row directly, so a test controls the
// project, content and revision without a canonical store.
func (ix *Index) SeedMirrorForTest(ctx context.Context, issueUID, projectUID, content string, revision int64) error {
	if ix.pg != nil {
		_, err := ix.db.ExecContext(ctx, `
			INSERT INTO issue_vector_mirror(issue_uid, project_uid, content, content_revision)
			VALUES($1, $2, $3, $4)
			ON CONFLICT(issue_uid) DO UPDATE SET project_uid = EXCLUDED.project_uid,
			  content = EXCLUDED.content, content_revision = EXCLUDED.content_revision`,
			issueUID, projectUID, content, revision)
		return err
	}
	_, err := ix.db.ExecContext(ctx, `
		INSERT INTO issue_mirror(issue_uid, project_uid, content, content_revision)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(issue_uid) DO UPDATE SET project_uid = excluded.project_uid,
		  content = excluded.content, content_revision = excluded.content_revision`,
		issueUID, projectUID, content, revision)
	return err
}

// MirrorDocForTest describes one mirror row plus the stamps it carries: Stamps
// maps generation key to the revision stamped there, which may be stale.
type MirrorDocForTest struct {
	IssueUID, ProjectUID, Content string
	Revision                      int64
	Stamps                        map[string]int64
}

// SeedMirrorDocsForTest inserts many mirror rows and stamps in one
// transaction, so property tests with hundreds of rows stay fast.
func (ix *Index) SeedMirrorDocsForTest(ctx context.Context, docs []MirrorDocForTest) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if ix.pg != nil {
		for _, d := range docs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO issue_vector_mirror(issue_uid, project_uid, content, content_revision)
				VALUES($1, $2, $3, $4)`, d.IssueUID, d.ProjectUID, d.Content, d.Revision); err != nil {
				return err
			}
			for key, revision := range d.Stamps {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO issue_vector_stamps(gen_key, issue_uid, revision) VALUES($1, $2, $3)`,
					key, d.IssueUID, revision); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE issue_vector_mirror SET embed_gen = $1 WHERE issue_uid = $2`, key, d.IssueUID); err != nil {
					return err
				}
			}
		}
		return tx.Commit()
	}
	for _, d := range docs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO issue_mirror(issue_uid, project_uid, content, content_revision)
			VALUES(?, ?, ?, ?)`, d.IssueUID, d.ProjectUID, d.Content, d.Revision); err != nil {
			return err
		}
		for key, revision := range d.Stamps {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR REPLACE INTO `+vectorsPrefix+`_stamps(ordinal, doc_key, revision)
				SELECT ordinal, ?, ? FROM `+vectorsPrefix+`_generations WHERE gen_key = ?`,
				d.IssueUID, revision, key); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE issue_mirror SET embed_gen = ? WHERE issue_uid = ?`, key, d.IssueUID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ClearMirrorForTest removes every mirror row and stamp.
func (ix *Index) ClearMirrorForTest(ctx context.Context) error {
	if ix.pg != nil {
		if _, err := ix.db.ExecContext(ctx, `DELETE FROM issue_vector_stamps`); err != nil {
			return err
		}
		_, err := ix.db.ExecContext(ctx, `DELETE FROM issue_vector_mirror`)
		return err
	}
	if _, err := ix.db.ExecContext(ctx, `DELETE FROM `+vectorsPrefix+`_stamps`); err != nil {
		return err
	}
	_, err := ix.db.ExecContext(ctx, `DELETE FROM issue_mirror`)
	return err
}
