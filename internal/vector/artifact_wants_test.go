package vector_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
	"hegel.dev/go/hegel"
)

// wantsBackends opens an index with one building generation on SQLite and on
// PostgreSQL (holding the reconciler lease, as the production caller does).
func wantsBackends(t *testing.T, check func(t *testing.T, ix *vector.Index, key string, postgres bool)) {
	t.Helper()
	gen := kitvec.Generation{Model: "example-model", Dimensions: 2}
	key := gen.Fingerprint()
	t.Run("sqlite", func(t *testing.T) {
		ix, err := vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, ix.Close()) })
		require.NoError(t, ix.EnsureBuilding(t.Context(), key, gen))
		check(t, ix, key, false)
	})
	t.Run("postgres", func(t *testing.T) {
		ctx := t.Context()
		dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
		t.Cleanup(cleanup)
		store, err := pgstore.Open(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		ix, err := vector.OpenPostgres(ctx, store.DB)
		require.NoError(t, err)
		release, err := ix.AcquireReconcilerLease(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, release()) })
		require.NoError(t, ix.EnsureBuilding(ctx, key, gen))
		check(t, ix, key, true)
	})
}

type stampState int

const (
	unstamped stampState = iota
	stampedCurrent
	stampedStale
)

type wantsDoc struct {
	project, issue, content string
	state                   stampState
}

const wantsUIDAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func TestPendingArtifactWantsPerProjectBound(t *testing.T) {
	wantsBackends(t, func(t *testing.T, ix *vector.Index, key string, postgres bool) {
		other := kitvec.Generation{Model: "other-model", Dimensions: 2}
		require.NoError(t, ix.EnsureBuilding(t.Context(), other.Fingerprint(), other))
		uidGen := hegel.Text().Alphabet(wantsUIDAlphabet).MinSize(1).MaxSize(8)
		hegel.Test(t, func(ht *hegel.T) {
			ctx := context.Background()
			require.NoError(ht, ix.ClearMirrorForTest(ctx))
			usedProjects, usedIssues := map[string]bool{}, map[string]bool{}
			var docs []wantsDoc
			for range hegel.Draw(ht, hegel.Integers(1, 6)) {
				project := hegel.Draw(ht, uidGen)
				if usedProjects[project] {
					continue
				}
				usedProjects[project] = true
				for range hegel.Draw(ht, hegel.Integers(0, 80)) {
					issue := hegel.Draw(ht, uidGen)
					if usedIssues[issue] {
						continue
					}
					usedIssues[issue] = true
					content := hegel.Draw(ht, hegel.Text())
					if postgres {
						content = strings.ReplaceAll(content, "\x00", "") // PostgreSQL text cannot hold NUL
					}
					docs = append(docs, wantsDoc{
						project: project, issue: issue, content: content,
						state: stampState(hegel.Draw(ht, hegel.Integers(0, 2))),
					})
				}
			}
			seed := make([]vector.MirrorDocForTest, len(docs))
			for i, d := range docs {
				revision := int64(i + 1)
				seed[i] = vector.MirrorDocForTest{IssueUID: d.issue, ProjectUID: d.project, Content: d.content, Revision: revision, Stamps: map[string]int64{}}
				switch d.state {
				case stampedCurrent:
					seed[i].Stamps[key] = revision
				case stampedStale:
					seed[i].Stamps[key] = revision + 1000
				}
				// A stamp under another generation never covers this one.
				if hegel.Draw(ht, hegel.Booleans()) {
					seed[i].Stamps[other.Fingerprint()] = revision
				}
			}
			require.NoError(ht, ix.SeedMirrorDocsForTest(ctx, seed))
			perProject := hegel.Draw(ht, hegel.Integers(1, 100))
			// Cursors may name projects with no documents, and a cursor may be
			// empty, below, inside or above a project's uid range.
			after := map[string]string{}
			for range hegel.Draw(ht, hegel.Integers(0, 6)) {
				project := hegel.Draw(ht, hegel.SampledFrom(append([]string{"absent-project"}, projectNames(usedProjects)...)))
				after[project] = hegel.Draw(ht, hegel.Text().Alphabet(wantsUIDAlphabet).MaxSize(8))
			}
			scanLimit := hegel.Draw(ht, hegel.Integers(1, 600))
			afterProject := hegel.Draw(ht, hegel.Text().Alphabet(wantsUIDAlphabet).MaxSize(8))

			got, err := ix.PendingArtifactWantsAfterProject(ctx, key, perProject, scanLimit, after, afterProject)
			require.NoError(ht, err)

			// Independent model: select a bounded, rotated set of projects,
			// then return one bounded page from each project. An exhausted
			// project cursor wraps to that project's first pending document.
			byProject := map[string][]wantsDoc{}
			for _, d := range docs {
				if d.state != stampedCurrent {
					byProject[d.project] = append(byProject[d.project], d)
				}
			}
			projects := make([]string, 0, len(byProject))
			for project := range byProject {
				projects = append(projects, project)
			}
			slices.SortFunc(projects, func(a, b string) int {
				afterA, afterB := a > afterProject, b > afterProject
				if afterA != afterB {
					if afterA {
						return -1
					}
					return 1
				}
				return strings.Compare(a, b)
			})
			pageSize := min(perProject, scanLimit)
			projectLimit := max(1, scanLimit/pageSize)
			var kept []wantsDoc
			for _, project := range projects[:min(len(projects), projectLimit)] {
				group := byProject[project]
				slices.SortFunc(group, func(a, b wantsDoc) int { return strings.Compare(a.issue, b.issue) })
				eligible := make([]wantsDoc, 0, len(group))
				for _, d := range group {
					if d.issue > after[project] {
						eligible = append(eligible, d)
					}
				}
				if len(eligible) == 0 && after[project] != "" {
					eligible = group
				}
				kept = append(kept, eligible[:min(len(eligible), pageSize)]...)
			}
			want := []vector.ArtifactWant{}
			for _, d := range kept {
				want = append(want, vector.ArtifactWant{IssueUID: d.issue, ProjectUID: d.project, InputHash: embedding.ArtifactInputHash(d.content)})
			}
			require.Equal(ht, want, got)
		}, hegel.WithTestCases(60))
	})
}

func TestPendingArtifactWantsOneLargeProjectDoesNotStarve(t *testing.T) {
	wantsBackends(t, func(t *testing.T, ix *vector.Index, key string, _ bool) {
		ctx := t.Context()
		for i := range 300 {
			require.NoError(t, ix.SeedMirrorForTest(ctx, fmt.Sprintf("a%04d", i), "hub-project-a", "content", 1))
		}
		require.NoError(t, ix.SeedMirrorForTest(ctx, "b0000", "hub-project-b", "other", 1))

		got, err := ix.PendingArtifactWants(ctx, key, 32, 4096, nil)
		require.NoError(t, err)
		require.Len(t, got, 33)
		require.Equal(t, vector.ArtifactWant{IssueUID: "b0000", ProjectUID: "hub-project-b", InputHash: embedding.ArtifactInputHash("other")}, got[32])

		got, err = ix.PendingArtifactWantsAfterProject(ctx, key, 32, 40, nil, "hub-project-a")
		require.NoError(t, err)
		require.Equal(t, []vector.ArtifactWant{{IssueUID: "b0000", ProjectUID: "hub-project-b", InputHash: embedding.ArtifactInputHash("other")}}, got)
	})
}

func TestPendingArtifactWantsProjectCursorRotatesPastGlobalPrefix(t *testing.T) {
	wantsBackends(t, func(t *testing.T, ix *vector.Index, key string, _ bool) {
		ctx := t.Context()
		const projectCount, docsPerProject = 133, 31
		docs := make([]vector.MirrorDocForTest, 0, projectCount*docsPerProject)
		for project := range projectCount {
			projectUID := fmt.Sprintf("project-%03d", project)
			for issue := range docsPerProject {
				docs = append(docs, vector.MirrorDocForTest{
					IssueUID:   fmt.Sprintf("issue-%03d-%02d", project, issue),
					ProjectUID: projectUID,
					Content:    "unavailable at the hub",
					Revision:   int64(issue + 1),
					Stamps:     map[string]int64{},
				})
			}
		}
		require.NoError(t, ix.SeedMirrorDocsForTest(ctx, docs))

		first, err := ix.PendingArtifactWantsAfterProject(ctx, key, 32, 4096, nil, "")
		require.NoError(t, err)
		require.Len(t, first, 128*docsPerProject)
		require.Equal(t, "project-127", first[len(first)-1].ProjectUID)
		second, err := ix.PendingArtifactWantsAfterProject(ctx, key, 32, 4096, nil, first[len(first)-1].ProjectUID)
		require.NoError(t, err)
		require.Len(t, second, 128*docsPerProject)

		visited := map[string]bool{}
		for _, want := range append(first, second...) {
			visited[want.ProjectUID] = true
		}
		require.Len(t, visited, projectCount, "project rotation must reach projects past the repeated 4096-want prefix")
		require.True(t, visited["project-132"])
	})
}

func TestPendingArtifactWantsFollowsStampsAndRevisions(t *testing.T) {
	wantsBackends(t, func(t *testing.T, ix *vector.Index, key string, _ bool) {
		ctx := t.Context()
		require.NoError(t, ix.SeedMirrorForTest(ctx, "i1", "spoke-project", "one", 1))
		require.NoError(t, ix.SeedMirrorForTest(ctx, "i2", "spoke-project", "two", 1))
		require.NoError(t, ix.SaveVectorsForTest(ctx, key, "i1", int64(1), []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0}}}))

		got, err := ix.PendingArtifactWants(ctx, key, 32, 4096, nil)
		require.NoError(t, err)
		require.Equal(t, []string{"i2"}, wantUIDs(got))

		// An edit bumps the revision, so the stamped document is pending again.
		require.NoError(t, ix.SeedMirrorForTest(ctx, "i1", "spoke-project", "one edited", 2))
		got, err = ix.PendingArtifactWants(ctx, key, 32, 4096, nil)
		require.NoError(t, err)
		require.Equal(t, []string{"i1", "i2"}, wantUIDs(got))
		require.Equal(t, embedding.ArtifactInputHash("one edited"), got[0].InputHash)
	})
}

func TestPendingArtifactWantsOrdersMixedCaseUIDsByBytes(t *testing.T) {
	wantsBackends(t, func(t *testing.T, ix *vector.Index, key string, _ bool) {
		ctx := t.Context()
		for _, uid := range []string{"a", "B", "b", "Z"} {
			require.NoError(t, ix.SeedMirrorForTest(ctx, uid, "spoke-project", "text", 1))
		}
		got, err := ix.PendingArtifactWants(ctx, key, 2, 4096, nil)
		require.NoError(t, err)
		require.Equal(t, []string{"B", "Z"}, orderedUIDs(got), "per-project cap keeps the bytewise-lowest")
		got, err = ix.PendingArtifactWants(ctx, key, 32, 4096, nil)
		require.NoError(t, err)
		require.Equal(t, []string{"B", "Z", "a", "b"}, orderedUIDs(got))
	})
}

func orderedUIDs(wants []vector.ArtifactWant) []string {
	out := make([]string, len(wants))
	for i, w := range wants {
		out[i] = w.IssueUID
	}
	return out
}

func TestPendingArtifactWantsNonPositiveBoundsReturnNothing(t *testing.T) {
	wantsBackends(t, func(t *testing.T, ix *vector.Index, key string, _ bool) {
		ctx := t.Context()
		require.NoError(t, ix.SeedMirrorForTest(ctx, "i1", "spoke-project", "one", 1))
		for _, bounds := range [][2]int{{0, 10}, {-1, 10}, {10, 0}, {10, -1}} {
			got, err := ix.PendingArtifactWants(ctx, key, bounds[0], bounds[1], nil)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Empty(t, got)
		}
	})
}

func wantUIDs(wants []vector.ArtifactWant) []string {
	out := make([]string, len(wants))
	for i, w := range wants {
		out[i] = w.IssueUID
	}
	slices.Sort(out)
	return out
}

func projectNames(used map[string]bool) []string {
	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestPendingArtifactWantsCursorSkipsReturnedPrefixPerProject(t *testing.T) {
	wantsBackends(t, func(t *testing.T, ix *vector.Index, key string, _ bool) {
		ctx := t.Context()
		for _, uid := range []string{"a1", "a2", "a3", "B1"} {
			require.NoError(t, ix.SeedMirrorForTest(ctx, uid, "hub-project-a", "text", 1))
		}
		require.NoError(t, ix.SeedMirrorForTest(ctx, "b1", "hub-project-b", "text", 1))
		require.NoError(t, ix.SeedMirrorForTest(ctx, "b2", "hub-project-b", "text", 1))

		// Project a resumes strictly after a1 (bytewise, so B1 sorts first and
		// is skipped); project b has no cursor and starts from the beginning.
		got, err := ix.PendingArtifactWants(ctx, key, 2, 4096, map[string]string{"hub-project-a": "a1", "unrelated-project": "zzz"})
		require.NoError(t, err)
		require.Equal(t, []string{"a2", "a3", "b1", "b2"}, orderedUIDs(got))

		// A cursor with no later pending documents wraps to the bytewise-first page.
		got, err = ix.PendingArtifactWants(ctx, key, 2, 4096, map[string]string{"hub-project-a": "a3"})
		require.NoError(t, err)
		require.Equal(t, []string{"B1", "a1", "b1", "b2"}, orderedUIDs(got))
	})
}
