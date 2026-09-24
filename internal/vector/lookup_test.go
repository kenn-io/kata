package vector

import (
	"context"
	"math"
	"strings"
	"testing"

	kitvec "go.kenn.io/kit/vector"
)

func TestLookupVectorsReturnsCoveredDocsWithContentHash(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	long := strings.Repeat("kata ", 1000) // > ChunkMaxRunes: several chunks
	seedMirrorRow(t, ix, "u1", "hub", long, 1)
	seedMirrorRow(t, ix, "u2", "hub", "pending text", 1)
	seedMirrorRow(t, ix, "u3", "other", "other project", 1)
	key := ensureTestGeneration(t, ix)
	if _, err := ix.FillExcluding(ctx, key, unitVectorEncoder(4), 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	seedMirrorRow(t, ix, "u2", "hub", "edited text", 2) // edit after the fill: no longer covered

	got, err := ix.LookupVectors(ctx, key, "hub", []string{"u1", "u2", "u3", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d documents, want only u1 (u2 stale, u3 other project, missing absent): %v", len(got), got)
	}
	u1 := got["u1"]
	if u1.ContentSHA256 != ContentSHA256(long) {
		t.Fatalf("hash = %s, want sha256 of mirror content", u1.ContentSHA256)
	}
	if len(u1.Chunks) < 2 {
		t.Fatalf("chunks = %d, want the multi-chunk document", len(u1.Chunks))
	}
	for i, c := range u1.Chunks {
		if c.ChunkIndex != i || len(c.Vector) != 4 || c.Vector[0] != 1 {
			t.Fatalf("chunk %d = %+v", i, c)
		}
	}
}

func TestLookupVectorsReportsStampOnlyDocsWithoutChunks(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirrorRow(t, ix, "u1", "hub", "rejected by provider", 1)
	key := ensureTestGeneration(t, ix)
	if err := ix.SaveImported(ctx, key, "u1", int64(1), nil); err != nil {
		t.Fatal(err)
	}
	got, err := ix.LookupVectors(ctx, key, "hub", []string{"u1"})
	if err != nil {
		t.Fatal(err)
	}
	if sv, ok := got["u1"]; !ok || len(sv.Chunks) != 0 {
		t.Fatalf("stamp-only doc = %+v, present = %v; want present with no chunks", sv, ok)
	}
}

func TestLookupVectorsUnknownGenerationIsEmpty(t *testing.T) {
	ix := openTestIndex(t)
	seedMirrorRow(t, ix, "u1", "hub", "text", 1)
	got, err := ix.LookupVectors(context.Background(), "never-ensured", "hub", []string{"u1"})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, err %v; want empty, nil", got, err)
	}
}

func TestVectorWireCodecRoundTrips(t *testing.T) {
	in := kitvec.Vector{1, -0.5, float32(math.SmallestNonzeroFloat32), 0}
	b := EncodeVector(in)
	if len(b) != 16 {
		t.Fatalf("encoded %d bytes, want 16", len(b))
	}
	out, err := DecodeVector(b, 4)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("component %d: %v != %v", i, out[i], in[i])
		}
	}
	if _, err := DecodeVector(b, 3); err == nil {
		t.Fatal("wrong dims must be rejected")
	}
}

func TestParsePostgresVectorText(t *testing.T) {
	v, err := parsePostgresVector("[0.5,-1,0]", 3)
	if err != nil || v[0] != 0.5 || v[1] != -1 || v[2] != 0 {
		t.Fatalf("v = %v, err = %v", v, err)
	}
	if _, err := parsePostgresVector("[0.5,1]", 3); err == nil {
		t.Fatal("dimension mismatch must fail")
	}
	if _, err := parsePostgresVector("0.5,1,2", 3); err == nil {
		t.Fatal("missing brackets must fail")
	}
}
