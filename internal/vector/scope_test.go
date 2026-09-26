package vector

import (
	"context"
	"errors"
	"slices"
	"testing"

	kitvec "go.kenn.io/kit/vector"
)

func seedMirrorRow(t *testing.T, ix *Index, uid, project, content string, rev int64) {
	t.Helper()
	if _, err := ix.db.ExecContext(context.Background(), `
		INSERT INTO issue_mirror (issue_uid, project_uid, content, content_revision)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(issue_uid) DO UPDATE SET
		  project_uid = excluded.project_uid,
		  content = excluded.content,
		  content_revision = excluded.content_revision`,
		uid, project, content, rev); err != nil {
		t.Fatal(err)
	}
}

func ensureTestGeneration(t *testing.T, ix *Index) string {
	t.Helper()
	g := testGen("m1")
	key := g.Fingerprint()
	if err := ix.EnsureBuilding(context.Background(), key, g); err != nil {
		t.Fatal(err)
	}
	return key
}

func pendingDocs(t *testing.T, ix *Index, key string, scope PendingScope, limit int) []string {
	t.Helper()
	docs, err := ix.PendingScoped(context.Background(), key, scope, limit)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Doc
	}
	return out
}

func unitVectorEncoder(dims int) kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			v := make([]float32, dims)
			v[0] = 1
			out[i] = v
		}
		return out, nil
	}
}

func TestPendingScopedFiltersByProjectAndCursor(t *testing.T) {
	ix := openTestIndex(t)
	seedMirrorRow(t, ix, "u1", "local", "a", 1)
	seedMirrorRow(t, ix, "u2", "hub", "b", 1)
	seedMirrorRow(t, ix, "u3", "hub", "c", 1)
	key := ensureTestGeneration(t, ix)

	if got := pendingDocs(t, ix, key, PendingScope{}, 10); !slices.Equal(got, []string{"u1", "u2", "u3"}) {
		t.Fatalf("unscoped = %v", got)
	}
	if got := pendingDocs(t, ix, key, PendingScope{Projects: []string{"hub"}}, 10); !slices.Equal(got, []string{"u1"}) {
		t.Fatalf("excluding hub = %v", got)
	}
	if got := pendingDocs(t, ix, key, PendingScope{Projects: []string{"hub"}, Include: true}, 10); !slices.Equal(got, []string{"u2", "u3"}) {
		t.Fatalf("only hub = %v", got)
	}
	if got := pendingDocs(t, ix, key, PendingScope{Projects: []string{"hub"}, Include: true, AfterDoc: "u2"}, 10); !slices.Equal(got, []string{"u3"}) {
		t.Fatalf("hub after u2 = %v", got)
	}
	if got := pendingDocs(t, ix, key, PendingScope{Include: true}, 10); len(got) != 0 {
		t.Fatalf("include with no projects must select nothing, got %v", got)
	}
}

// PendingScoped forks kit's freshness predicate into kata SQL so it can filter
// by project. This pins the fork to kit's definition: unscoped, both must
// report exactly the same documents before and after stamping and editing.
func TestPendingScopedMatchesKitPending(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirrorRow(t, ix, "u1", "p", "a", 1)
	seedMirrorRow(t, ix, "u2", "p", "b", 1)
	key := ensureTestGeneration(t, ix)
	check := func(stage string) {
		t.Helper()
		kit, err := ix.store.PendingForGeneration(ctx, key, 10)
		if err != nil {
			t.Fatal(err)
		}
		want := make([]string, len(kit))
		for i, p := range kit {
			want[i] = p.Doc
		}
		if got := pendingDocs(t, ix, key, PendingScope{}, 10); !slices.Equal(got, want) {
			t.Fatalf("%s: PendingScoped = %v, kit = %v", stage, got, want)
		}
	}
	check("fresh")
	if err := ix.SaveImported(ctx, key, "u1", int64(1), []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	check("after stamp")
	seedMirrorRow(t, ix, "u1", "p", "a2", 2)
	check("after edit")
}

func TestFillExcludingSkipsExcludedProjects(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirrorRow(t, ix, "u1", "local", "local text", 1)
	seedMirrorRow(t, ix, "u2", "hub", "hub text", 1)
	key := ensureTestGeneration(t, ix)

	var encoded []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		return unitVectorEncoder(4)(ctx, texts)
	}
	stats, err := ix.FillExcluding(ctx, key, enc, 0, nil, nil, []string{"hub"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != 1 || !slices.Equal(encoded, []string{"local text"}) {
		t.Fatalf("stats = %+v, encoded = %q; want only the local row", stats, encoded)
	}
	if got := pendingDocs(t, ix, key, PendingScope{}, 10); !slices.Equal(got, []string{"u2"}) {
		t.Fatalf("pending after scoped fill = %v, want the excluded row", got)
	}
}

func TestSaveImportedStampsAndRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirrorRow(t, ix, "u1", "hub", "text", 3)
	seedMirrorRow(t, ix, "u2", "hub", "skipped text", 1)
	key := ensureTestGeneration(t, ix)

	err := ix.SaveImported(ctx, key, "u1", int64(2), []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0, 0}}})
	if !errors.Is(err, kitvec.ErrStale) {
		t.Fatalf("stale revision: err = %v, want ErrStale", err)
	}
	if err := ix.SaveImported(ctx, key, "u1", int64(3), []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := ix.SaveImported(ctx, key, "u2", int64(1), nil); err != nil {
		t.Fatal(err)
	}
	embedded, skipped, backlog, err := ix.Coverage(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if embedded != 1 || skipped != 1 || backlog != 0 {
		t.Fatalf("coverage = %d/%d/%d, want 1 embedded, 1 skipped, 0 backlog", embedded, skipped, backlog)
	}
}

func TestGenerationStateReportsRegisteredKeys(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	if state, err := ix.GenerationState(ctx, "missing"); err != nil || state != "" {
		t.Fatalf("missing key: state = %q, err = %v", state, err)
	}
	key := ensureTestGeneration(t, ix)
	if state, err := ix.GenerationState(ctx, key); err != nil || state != "building" {
		t.Fatalf("ensured key: state = %q, err = %v", state, err)
	}
	if err := ix.CutOver(ctx, key); err != nil {
		t.Fatal(err)
	}
	if state, err := ix.GenerationState(ctx, key); err != nil || state != "active" {
		t.Fatalf("cut-over key: state = %q, err = %v", state, err)
	}
}

func TestContentSHA256IsLowercaseHex(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := ContentSHA256("abc"); got != want {
		t.Fatalf("ContentSHA256(abc) = %s, want %s", got, want)
	}
}
