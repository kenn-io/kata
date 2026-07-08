package vector

import (
	"context"
	"testing"
)

// testGen and seedMirror live in fill_test.go, shared across both test files.

func fillAll(t *testing.T, ix *Index, key string) {
	t.Helper()
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0, 0}
		}
		return out, nil
	}
	if _, err := ix.Fill(context.Background(), key, enc, 0, 0); err != nil {
		t.Fatalf("fill: %v", err)
	}
}

func TestLifecycleEnsureCutOverBacklog(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirror(t, ix, "u1", 1)

	g1 := testGen("m1")
	k1 := g1.Fingerprint()
	if err := ix.EnsureBuilding(ctx, k1, g1); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ix.ActiveGeneration(ctx); ok {
		t.Fatal("no generation should be active before cutover")
	}
	if n, err := ix.Backlog(ctx, k1); err != nil || n != 1 {
		t.Fatalf("backlog = %d, %v; want 1", n, err)
	}
	fillAll(t, ix, k1)
	if n, _ := ix.Backlog(ctx, k1); n != 0 {
		t.Fatalf("backlog after fill = %d, want 0", n)
	}
	if err := ix.CutOver(ctx, k1); err != nil {
		t.Fatal(err)
	}
	if key, ok, _ := ix.ActiveGeneration(ctx); !ok || key != k1 {
		t.Fatalf("active = %q, %v; want %q", key, ok, k1)
	}
	if err := ix.CutOver(ctx, k1); err != nil { // idempotent no-op
		t.Fatal(err)
	}

	// Model change: g2 builds while g1 stays active, then cutover retires g1.
	g2 := testGen("m2")
	k2 := g2.Fingerprint()
	if err := ix.EnsureBuilding(ctx, k2, g2); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := ix.ActiveGeneration(ctx); key != k1 {
		t.Fatalf("g1 must stay active during g2 build, active = %q", key)
	}
	fillAll(t, ix, k2)
	if err := ix.CutOver(ctx, k2); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := ix.ActiveGeneration(ctx); key != k2 {
		t.Fatalf("active = %q, want %q", key, k2)
	}
	// g1's vec0 table and stamps are reclaimed.
	var n int
	if err := ix.db.QueryRowContext(ctx, `
		SELECT count(*) FROM issue_vectors_stamps s
		JOIN issue_vectors_generations g ON g.ordinal = s.ordinal
		WHERE g.gen_key = ?`, k1).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("retired generation stamps = %d, want 0", n)
	}
	// EnsureBuilding on the active generation must not demote it.
	if err := ix.EnsureBuilding(ctx, k2, g2); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := ix.ActiveGeneration(ctx); key != k2 {
		t.Fatal("EnsureBuilding demoted the active generation")
	}
}
