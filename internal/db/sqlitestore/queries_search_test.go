package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSearchFTS_NormalizedDuplicateScores(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "login cafe pull request", "")
	for name, search := range map[string]func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){
		"all": d.SearchFTS, "any": d.SearchFTSAny,
	} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct{ query, repeated string }{
				{"login", "Login login"},
				{"cafe", "café cafe"},
				{"login", "login, login"},
				{"pull-request", "pull-request pull.request"},
			} {
				t.Run(tc.repeated, func(t *testing.T) {
					base, err := search(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: tc.query})
					require.NoError(t, err)
					require.Len(t, base, 1)
					hits, err := search(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: tc.repeated})
					require.NoError(t, err)
					require.Len(t, hits, 1)
					assert.Equal(t, issue.ID, hits[0].Issue.ID)
					assert.Equal(t, base[0].Score, hits[0].Score, "normalized duplicates must contribute one score")
				})
			}
		})
	}
}

func TestFTS_IssueUpdateReindexes(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "old title", "initial body content")

	newTitle := "fresh title"
	_, _, _, err := d.EditIssue(ctx, db.EditIssueParams{
		IssueID: issue.ID, Title: &newTitle, Actor: "tester",
	})
	require.NoError(t, err)

	hits, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "old"})
	require.NoError(t, err)
	require.Empty(t, hits)
	hits, err = d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "fresh"})
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

func TestFTS_CommentUpdateReindexes(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "boring", "body")
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "tester", Body: "leakedtoken",
	})
	require.NoError(t, err)

	_, _, _, err = d.EditComment(ctx, db.EditCommentParams{
		IssueID: issue.ID, CommentUID: comment.UID, Actor: "tester", Body: "redactedtoken",
	})
	require.NoError(t, err)

	hits, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "leakedtoken"})
	require.NoError(t, err)
	require.Empty(t, hits)
	hits, err = d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "redactedtoken"})
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

func TestSearchFTS_RanksByBM25(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)

	// Three issues. Only the first two mention "login"; the second has it
	// many more times. Issue 1's body is padded with unrelated text so token
	// density makes issue 2's BM25 win unambiguous regardless of column-length
	// normalization quirks.
	createTesterIssueWithBody(ctx, t, d, p.ID, "fix login crash",
		"stack trace from a totally unrelated incident with many tokens to dilute density and dominate length normalization here")
	createTesterIssueWithBody(ctx, t, d, p.ID, "login is broken on login screen",
		"login fails twice login login login")
	createTesterIssueWithBody(ctx, t, d, p.ID, "unrelated issue", "no match here")

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login", Limit: 20})
	require.NoError(t, err)
	assert.Len(t, got, 2)
	// The doubly-mentioned issue should outrank the singly-mentioned one.
	// Issue.Number removed in Task 3; ranking verified by position only.
	_ = got[0].Issue.ShortID
	_ = got[1].Issue.ShortID
	// Issue 2 has "login" in both title and body; issue 1 has it only in title.
	assert.Equal(t, []string{"title", "body"}, got[0].MatchedIn,
		"issue 2 has login in title AND body")
	assert.Equal(t, []string{"title"}, got[1].MatchedIn,
		"issue 1 has login only in title")
}

func TestSearchFTS_MatchedIn_Comments(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)

	// Issue body does NOT contain the search term; only the comment does.
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "unrelated", "nothing here")
	addTesterComment(ctx, t, d, issue.ID, "watermelon found")

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "watermelon", Limit: 20})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"comments"}, got[0].MatchedIn,
		"term appears only in a comment")
}

func TestSearchFTS_MatchedIn_AllThreeColumns(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)

	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "watermelon title", "watermelon body")
	addTesterComment(ctx, t, d, issue.ID, "watermelon comment")

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "watermelon", Limit: 20})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"title", "body", "comments"}, got[0].MatchedIn,
		"matched_in must list columns in title/body/comments order")
}

func TestSearchFTS_FiltersByProject(t *testing.T) {
	t.Parallel()
	d, ctx, p1 := setupTestProject(t)
	p2 := createProject(ctx, t, d, "p2")

	createTesterIssueWithBody(ctx, t, d, p1.ID, "login bug", "")
	createTesterIssueWithBody(ctx, t, d, p2.ID, "login bug", "")

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p1.ID, Query: "login", Limit: 20})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, p1.ID, got[0].Issue.ProjectID)
}

func TestSearchFTS_PopulatesIssueUIDs(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "login bug", "")

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login", Limit: 20})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, issue.UID, got[0].Issue.UID)
	assert.Equal(t, p.UID, got[0].Issue.ProjectUID)
}

func TestSearchFTS_ExcludesDeletedByDefault(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	keep := createTesterIssueWithBody(ctx, t, d, p.ID, "keep login", "")
	gone := createTesterIssueWithBody(ctx, t, d, p.ID, "deleted login", "")
	// Mark the second issue soft-deleted directly via SQL — Task 5 ships
	// SoftDeleteIssue but this test runs before that, and the DB-layer
	// behavior we want to verify is "search query filters deleted_at IS NULL".
	_, err := d.ExecContext(ctx,
		`UPDATE issues SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		gone.ID)
	require.NoError(t, err)

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login", Limit: 20})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, keep.ID, got[0].Issue.ID, "soft-deleted issue must be filtered")

	// includeDeleted=true returns both.
	got, err = d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login", Limit: 20, IncludeDeleted: true})
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestSearchFTS_EmptyQueryReturnsEmpty(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	createTesterIssueWithBody(ctx, t, d, p.ID, "anything", "")

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "   ", Limit: 20})
	require.NoError(t, err)
	assert.Empty(t, got, "blank query → empty result, not an error")
}

func TestSearchFTS_LimitCappedAt200(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	// Create 250 matching issues. The cap must clamp the result set to 200
	// regardless of how large a limit the caller passes; without the cap, this
	// test would return 250 and fail.
	for range 250 {
		createTesterIssueWithBody(ctx, t, d, p.ID, "login bug", "")
	}
	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login", Limit: 1_000_000})
	require.NoError(t, err)
	assert.Len(t, got, 200, "limit must be capped at 200")
}

func TestSearchFTS_MultiTermImplicitAND(t *testing.T) {
	t.Parallel()
	// Multi-term queries split on whitespace and AND the terms. "login Safari"
	// must match an issue that contains both terms even when they aren't
	// adjacent — the previous single-phrase wrap missed this.
	d, ctx, p := setupTestProject(t)

	createTesterIssue(ctx, t, d, p.ID, "fix login crash on Safari")
	createTesterIssueWithBody(ctx, t, d, p.ID, "login bug", "")
	createTesterIssueWithBody(ctx, t, d, p.ID, "Safari preferences", "")

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login Safari", Limit: 20})
	require.NoError(t, err)
	require.Len(t, got, 1, "only the issue containing both terms must match")
	assert.Equal(t, "fix login crash on Safari", got[0].Issue.Title)
}

func TestSearchFTS_RareWordAND(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	title := createTesterIssueWithBody(ctx, t, d, p.ID, "quasar login browser", "")
	body := createTesterIssueWithBody(ctx, t, d, p.ID, "ordinary task", "quasar login browser")
	comment := createTesterIssueWithBody(ctx, t, d, p.ID, "login", "browser")
	addTesterComment(ctx, t, d, comment.ID, "quasar login browser")
	// The rare word also occurs on an issue missing one required word.
	createTesterIssueWithBody(ctx, t, d, p.ID, "quasar login", "")
	for range 20 {
		issue := createTesterIssueWithBody(ctx, t, d, p.ID, "login browser", "")
		addTesterComment(ctx, t, d, issue.ID, "login browser")
	}

	// Hand-calculated BM25 with k1=1.2 and b=0.75. Issues have 24
	// documents and 52 tokens; comments have 21 documents and 43 tokens.
	// Quasar's IDF is ln(21.5/3.5) in issues and ln(20.5/1.5) in
	// comments. Login and browser use the 1e-6 IDF floor in both indexes.
	want := []struct {
		id    int64
		flags []string
		score float64
	}{
		{comment.ID, []string{"title", "body", "comments"}, 2.1969398536340305},
		{title.ID, []string{"title"}, 1.5685000074276114},
		{body.ID, []string{"body"}, 1.1826275682426863},
	}
	hits, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login browser quasar"})
	require.NoError(t, err)
	require.Len(t, hits, len(want))
	for i, hit := range hits {
		assert.Equal(t, want[i].id, hit.Issue.ID)
		assert.Equal(t, want[i].flags, hit.MatchedIn)
		assert.InDelta(t, want[i].score, hit.Score, 1e-12)
	}
}

func TestSearchFTS_QueryEscaping(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	createTesterIssueWithBody(ctx, t, d, p.ID, `fix "login" crash`, "")

	// FTS5 syntax characters must not surface as syntax errors.
	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: `"login"`, Limit: 20})
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

// TestSearchFTS_MatchedIn_CrossColumn pins the matched_in fix from roborev
// 16789: when the query is multi-term implicit-AND and the tokens land in
// different columns (title="login", body="Safari"), the per-column subqueries
// must use OR semantics so each column reports as matched. With the old
// AND-based per-column subqueries this returned matched_in=[].
func TestSearchFTS_MatchedIn_CrossColumn(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	createTesterIssueWithBody(ctx, t, d, p.ID, "login bug", "Safari issue")

	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login Safari", Limit: 20})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.ElementsMatch(t, []string{"title", "body"}, got[0].MatchedIn)
}

// TestSearchFTS_OperatorWordsAsLiterals pins #23: FTS5 reserved words and
// special characters in the user query must be treated as literal phrase
// content, not as FTS5 syntax. The escaping pass quotes each whitespace-split
// token and doubles embedded quotes, which neutralizes NEAR/OR/AND/NOT and
// the `*` prefix-match marker.
func TestSearchFTS_OperatorWordsAsLiterals(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	createTesterIssueWithBody(ctx, t, d, p.ID, "do NOT merge yet", "blocked OR waiting")
	createTesterIssue(ctx, t, d, p.ID, "merge after review")
	// Seeded so AND/NEAR-as-literals can be exercised: each operator-bearing
	// row pairs with a competitor that has the surrounding tokens but lacks
	// the operator word itself. Under operator semantics ("build" AND
	// "deploy") both rows would match; under correct literal semantics only
	// the row containing the literal operator word matches. The test asserts
	// exactly one match, so a regression that lost the FTS5 phrase-quoting
	// fails here.
	createTesterIssue(ctx, t, d, p.ID, "build AND deploy pipeline")
	createTesterIssue(ctx, t, d, p.ID, "build deploy pipeline")
	createTesterIssue(ctx, t, d, p.ID, "place item NEAR exit")
	createTesterIssue(ctx, t, d, p.ID, "place item by exit")

	cases := []struct {
		name, query, wantTitle string
	}{
		{"NOT as literal", "NOT merge", "do NOT merge yet"},
		{"OR as literal", "blocked OR", "do NOT merge yet"},
		{"AND as literal", "build AND deploy", "build AND deploy pipeline"},
		{"NEAR as literal", "item NEAR exit", "place item NEAR exit"},
		{"star is literal not prefix", "merg*", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: tc.query, Limit: 20})
			require.NoError(t, err, "query %q must parse without FTS5 syntax error", tc.query)
			if tc.wantTitle == "" {
				assert.Len(t, got, 0, "wildcard must not act as prefix-match")
				return
			}
			require.Len(t, got, 1, "query %q must match exactly one row", tc.query)
			assert.Equal(t, tc.wantTitle, got[0].Issue.Title)
		})
	}
}

// TestSearchFTSAny_FindsNearDuplicates pins the look-alike fix from roborev
// 16791: implicit-AND search misses a row that's a near-duplicate but lacks
// one of the query tokens; the OR variant retrieves it so similarity.Score
// can decide. With AND, "login crash Safari critical" excludes a row that
// only contains "login crash Safari" (no "critical").
func TestSearchFTSAny_FindsNearDuplicates(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	createTesterIssueWithBody(ctx, t, d, p.ID, "login crash on Safari", "stack trace")

	// Implicit-AND: query has an extra token ("critical") that the row lacks
	// — the row is filtered out before similarity scoring would see it.
	gotAnd, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login crash Safari critical", Limit: 20})
	require.NoError(t, err)
	assert.Len(t, gotAnd, 0, "AND form must filter rows missing any token")

	// OR variant: same query, but the row matches because it contains at
	// least one of the tokens. similarity.Score in the handler decides
	// whether to actually treat this as a duplicate.
	gotOr, err := d.SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login crash Safari critical", Limit: 20})
	require.NoError(t, err)
	require.Len(t, gotOr, 1, "OR form must retrieve near-duplicates")
	assert.Equal(t, "login crash on Safari", gotOr[0].Issue.Title)
}

func TestSearchFTS_SeparateCommentDocuments(t *testing.T) {
	t.Parallel()
	d, ctx, p := setupTestProject(t)
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "login", "browser")
	addTesterComment(ctx, t, d, issue.ID, "safari pull")
	addTesterComment(ctx, t, d, issue.ID, "request timeout")
	var wordScores, titleBodyScores float64
	for _, word := range []string{"login", "browser", "safari", "timeout"} {
		got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: word})
		require.NoError(t, err)
		require.Len(t, got, 1)
		wordScores += got[0].Score
		if word == "login" || word == "browser" {
			titleBodyScores += got[0].Score
		}
	}
	for name, search := range map[string]func(context.Context, db.SearchFTSParams) ([]db.SearchCandidate, error){"all": d.SearchFTS, "any": d.SearchFTSAny} {
		got, err := search(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login browser safari timeout"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		want := wordScores
		if name == "any" {
			want = titleBodyScores
		}
		require.InDelta(t, want, got[0].Score, 1e-12, "each word contributes its best eligible document score")
	}
	got, err := d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "pull-request"})
	require.NoError(t, err)
	require.Empty(t, got, "phrases cannot span comments")
	addTesterComment(ctx, t, d, issue.ID, "pull request")
	got, err = d.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "pull-request"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, []string{"comments"}, got[0].MatchedIn)
}
