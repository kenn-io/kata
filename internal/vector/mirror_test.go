package vector

import (
	"context"
	"path/filepath"
	"testing"

	"go.kenn.io/kata/internal/db"
)

type fakeStorage struct {
	db.Storage // nil embed: only ListIssueContent may be called
	issues     []db.IssueContent
}

func (f *fakeStorage) ListIssueContent(_ context.Context, afterID int64, limit int) ([]db.IssueContent, error) {
	var out []db.IssueContent
	for _, ic := range f.issues {
		if ic.ID > afterID {
			out = append(out, ic)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func openTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(context.Background(), filepath.Join(t.TempDir(), "vectors.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func TestMirrorContentReadsTheSeededDocument(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirror(t, ix, "u1", 1)

	got, err := ix.mirrorContent(ctx, "u1")
	if err != nil {
		t.Fatalf("mirrorContent: %v", err)
	}
	if got != "text" {
		t.Fatalf("mirrorContent = %q, want %q", got, "text")
	}
}

func TestMirrorContentErrorsForAnUnknownDocument(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)

	if _, err := ix.mirrorContent(ctx, "missing"); err == nil {
		t.Fatal("mirrorContent for an unmirrored document must error so the fill aborts " +
			"instead of stamping the document as skipped")
	}
}

// TestClosePostgresIndexLeavesTheCallersPoolOpen pins that a Postgres-backed
// Index does not close a handle it does not own. The sidecar handle stands in
// for the caller's pool here so the test needs no database container.
func TestClosePostgresIndexLeavesTheCallersPoolOpen(t *testing.T) {
	ctx := context.Background()
	owner := openTestIndex(t)
	borrowed := &Index{db: owner.db, pg: &postgresIndex{db: owner.db}}

	if err := borrowed.Close(); err != nil {
		t.Fatalf("Close on a Postgres-backed index: %v", err)
	}
	if err := owner.db.PingContext(ctx); err != nil {
		t.Fatalf("caller's pool was closed by Index.Close: %v", err)
	}
}

func TestRefreshMirrorUpdatesProjectMoveWithoutRevisionBump(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	st := &fakeStorage{issues: []db.IssueContent{
		{ID: 1, UID: "u1", ProjectUID: "p1", Title: "a", Body: "b", ContentRevision: 1},
	}}
	if _, err := ix.RefreshMirror(ctx, st); err != nil {
		t.Fatal(err)
	}

	// Move the issue to another project without a content edit: same
	// content_revision, changed ProjectUID. The mirror row must follow.
	st.issues[0].ProjectUID = "p2"
	if _, err := ix.RefreshMirror(ctx, st); err != nil {
		t.Fatal(err)
	}
	var projectUID string
	if err := ix.db.QueryRowContext(ctx,
		`SELECT project_uid FROM issue_mirror WHERE issue_uid = 'u1'`).Scan(&projectUID); err != nil {
		t.Fatal(err)
	}
	if projectUID != "p2" {
		t.Fatalf("project_uid = %q, want %q after a project move with unchanged revision", projectUID, "p2")
	}
}

func TestRefreshMirrorUpsertsAndDeletes(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	st := &fakeStorage{issues: []db.IssueContent{
		{ID: 1, UID: "u1", ProjectUID: "p1", Title: "a", Body: "b", ContentRevision: 1},
		{ID: 2, UID: "u2", ProjectUID: "p1", Title: "c", Body: "d", ContentRevision: 1},
	}}
	if _, err := ix.RefreshMirror(ctx, st); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := ix.db.QueryRowContext(ctx,
		`SELECT content FROM issue_mirror WHERE issue_uid = 'u1'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "a\n\nb" {
		t.Fatalf("content = %q, want recipe-rendered text", content)
	}

	// Edit u1, delete u2.
	st.issues = []db.IssueContent{
		{ID: 1, UID: "u1", ProjectUID: "p1", Title: "a2", Body: "b", ContentRevision: 2},
	}
	if _, err := ix.RefreshMirror(ctx, st); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM issue_mirror`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("mirror rows = %d, want 1 (u2 deleted)", n)
	}
	var rev int64
	if err := ix.db.QueryRowContext(ctx,
		`SELECT content_revision FROM issue_mirror WHERE issue_uid = 'u1'`).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if rev != 2 {
		t.Fatalf("revision = %d, want 2", rev)
	}
}
