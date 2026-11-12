package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/similarity"
)

// Look-alike retrieval runs on issue creation and must stay cheap as comments grow.
func TestSearchFTSAny_LookalikePageBudget(t *testing.T) {
	const common = "login browser timeout session request response server client token account password reset"
	wideWords := make([]string, 140)
	for i := range wideWords {
		wideWords[i] = fmt.Sprintf("word%03d", i)
	}
	wideBody := strings.Join(wideWords, " ")
	cases := []struct {
		name          string
		title         string
		body          string
		query         string
		wordCount     int
		commentCount  int
		originalTitle string
		budget        int
		budgetReason  string
	}{
		{
			name:         "comments",
			title:        "login browser",
			body:         "safari timeout",
			query:        "login browser safari timeout session request response server client token account password reset user profile settings page button form submit redirect callback cookie header connection network retry error failure crash reload refresh expired invalid missing duplicate create update delete search issue title body comment project workspace author owner label status priority open closed task progress report review change fix support handle return result message details context window screen",
			wordCount:    68,
			commentCount: 30_000,
			budget:       20_000,
			budgetReason: "look-alike retrieval must rank title and body documents only",
		},
		{
			name:          "dense_postings",
			title:         "shared issue",
			body:          common,
			query:         common + " distinctive callback failure",
			wordCount:     15,
			originalTitle: "distinctive callback failure",
			budget:        20_000,
			budgetReason:  "look-alike retrieval must score each issue once",
		},
		{
			name:          "wide_dense_postings",
			title:         "shared issue",
			body:          wideBody,
			query:         wideBody + " distinctive callback failure",
			wordCount:     143,
			originalTitle: "distinctive callback failure",
			budget:        20_000,
			budgetReason:  "look-alike match flags must not re-read every match's postings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, path := openTestDBWithPath(t)
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			words := strings.Fields(tc.query)
			require.Len(t, words, tc.wordCount)
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			raw.SetMaxOpenConns(1)
			tx, err := raw.BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			insertIssue := prepareSeed(t, tx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`)
			for n := range 2_000 {
				_, err = insertIssue.ExecContext(ctx, fmt.Sprintf("%026d", n+1), project.ID, fmt.Sprintf("%08d", n+1), tc.title, tc.body)
				require.NoError(t, err)
			}
			insertComment := prepareSeed(t, tx, `INSERT INTO comments(uid,issue_id,author,body) VALUES(?,(SELECT id FROM issues WHERE project_id=? ORDER BY id LIMIT 1 OFFSET ?),'tester',?)`)
			for n := range tc.commentCount {
				start := n % (len(words) - 16)
				_, err = insertComment.ExecContext(ctx, fmt.Sprintf("%026d", n+1), project.ID, n%2_000, strings.Join(words[start:start+16], " "))
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			var original db.Issue
			if tc.originalTitle != "" {
				original = createTesterIssueWithBody(ctx, t, store, project.ID, tc.originalTitle, tc.body)
			}
			searchStore := &sqlitestore.Store{DB: raw}
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			start := time.Now()
			hits, err := searchStore.SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: tc.query, Limit: 20})
			elapsed := time.Since(start)
			require.NoError(t, err)
			require.Len(t, hits, 20)
			if tc.originalTitle != "" {
				require.Equal(t, original.ID, hits[0].Issue.ID)
			}
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			count := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			t.Logf("%d words / %d comments: %d page fetches, %.3f ms", len(words), tc.commentCount, count, float64(elapsed)/float64(time.Millisecond))
			require.LessOrEqual(t, count, tc.budget, tc.budgetReason)
		})
	}
}

func TestSearchFTSAny_LookalikeSizeAndOrder(t *testing.T) {
	for _, count := range []int{0, 5_100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := t.Context()
			store, path := openTestDBWithPath(t)
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			tx, err := raw.BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			insertIssue := prepareSeed(t, tx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`)
			for n := range count {
				_, err = insertIssue.ExecContext(ctx, fmt.Sprintf("%026d", n+1), project.ID, fmt.Sprintf("%08d", n+1), "unrelated issue", "other content")
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			const title = "login callback session timeout"
			const body = "request response token account"
			weaker := createTesterIssueWithBody(ctx, t, store, project.ID, title+" extra", body+" extra")
			bodyChanged := createTesterIssueWithBody(ctx, t, store, project.ID, title, body+" extra")
			exact := createTesterIssueWithBody(ctx, t, store, project.ID, title, body)
			params := db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), Limit: 1}
			baseline, err := store.SearchFTSAny(ctx, params)
			require.NoError(t, err)
			require.Equal(t, exact.ID, baseline[0].Issue.ID)
			params.TitleQuery = title
			params.TitleBody = body
			hits, err := store.SearchFTSAny(ctx, params)
			require.NoError(t, err)
			if count == 0 {
				require.Equal(t, baseline, hits, "small stores keep their ranking, limit, and metadata")
			} else {
				require.Equal(t, baseline, hits, "more warnings than Limit keep base ranking")
				params.Limit = 3
				hits, err = store.SearchFTSAny(ctx, params)
				require.NoError(t, err)
				require.Len(t, hits, 3)
				require.Equal(t, []int64{exact.ID, bodyChanged.ID, weaker.ID}, []int64{hits[0].Issue.ID, hits[1].Issue.ID, hits[2].Issue.ID})
				require.Equal(t, 1.0, hits[0].Score)
				require.InDelta(t, 0.92, hits[1].Score, 1e-12)
				require.InDelta(t, 0.8, hits[2].Score, 1e-12)
				require.Equal(t, []string{"title", "body"}, hits[0].MatchedIn)
				t.Run("twenty candidate limit", func(t *testing.T) {
					ctx := t.Context()
					for range 28 {
						createTesterIssueWithBody(ctx, t, store, project.ID, title, body+" alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon")
					}
					last := createTesterIssueWithBody(ctx, t, store, project.ID, title, body)
					params.Limit = 20
					hits, err := store.SearchFTSAny(ctx, params)
					require.NoError(t, err)
					require.Len(t, hits, 20)
					require.Equal(t, []int64{exact.ID, last.ID}, []int64{hits[0].Issue.ID, hits[1].Issue.ID}, "exact duplicates rank first, with ID breaking ties")
				})
				t.Run("twenty-one warnings keep base ranking", func(t *testing.T) {
					ctx := t.Context()
					const title = "archive export cursor checksum"
					const body = "payload destination"
					for range 21 {
						createTesterIssueWithBody(ctx, t, store, project.ID, title, body)
					}
					p := db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), Limit: 20}
					base, err := store.SearchFTSAny(ctx, p)
					require.NoError(t, err)
					require.Len(t, base, 20)
					p.TitleQuery, p.TitleBody = title, body
					hits, err := store.SearchFTSAny(ctx, p)
					require.NoError(t, err)
					require.Equal(t, base, hits, "21 warnings must retain base's top 20, scores, and flags")
				})
				for _, change := range []string{"project move", "subtree removal", "deletion"} {
					t.Run(change+" before hydration", func(t *testing.T) {
						ctx := t.Context()
						candidate := createTesterIssueWithBody(ctx, t, store, project.ID, title, body)
						params := db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), TitleQuery: title, TitleBody: body, AllowedIssueIDs: []int64{candidate.ID}}
						capture := &beforeQueryConnector{path: path, contains: "WHERE i.id IN (SELECT value FROM json_each(?))"}
						switch change {
						case "project move":
							target := createProject(ctx, t, store, "target-project")
							capture.beforeQuery = func() {
								_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: candidate.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: candidate.Revision, Actor: "tester"})
								require.NoError(t, err)
							}
						case "subtree removal":
							root := createTesterIssueWithBody(ctx, t, store, project.ID, "subtree root", "")
							link, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: candidate.ID, ToIssueID: root.ID, Type: "parent", Author: "tester"})
							require.NoError(t, err)
							params.IssueScope = &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, RootIssueUID: root.UID, ProjectUID: project.UID}
							capture.beforeQuery = func() {
								require.NoError(t, store.DeleteLinkByID(ctx, link.ID))
							}
						case "deletion":
							capture.beforeQuery = func() {
								_, _, _, err := store.SoftDeleteIssue(ctx, candidate.ID, "tester")
								require.NoError(t, err)
							}
						}
						hooked := sql.OpenDB(capture)
						t.Cleanup(func() { require.NoError(t, hooked.Close()) })
						hits, err := (&sqlitestore.Store{DB: hooked}).SearchFTSAny(ctx, params)
						require.NoError(t, err)
						require.Nil(t, capture.beforeQuery, "the mutation must run after selection and before hydration")
						require.Len(t, hits, 1, "selection and hydration must share one snapshot")
						require.Equal(t, candidate.ID, hits[0].Issue.ID)
						require.Equal(t, project.ID, hits[0].Issue.ProjectID)
						require.Nil(t, hits[0].Issue.DeletedAt)
						current, err := store.SearchFTSAny(ctx, params)
						require.NoError(t, err)
						require.Empty(t, current, "a later snapshot must see the committed visibility change")
					})
				}
				t.Run("non-ASCII title keeps base ranking", func(t *testing.T) {
					ctx := t.Context()
					const title = "ᲢᲔᲡᲢᲘ"
					issue := createTesterIssueWithBody(ctx, t, store, project.ID, title, body)
					p := db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), Limit: 20}
					base, err := store.SearchFTSAny(ctx, p)
					require.NoError(t, err)
					require.Equal(t, issue.ID, base[0].Issue.ID)
					capture := &beforeQueryConnector{path: path, contains: "SELECT i.id, i.title FROM issues_fts", beforeQuery: func() { t.Fatal("non-ASCII title must skip the title walk") }}
					raw := sql.OpenDB(capture)
					t.Cleanup(func() { require.NoError(t, raw.Close()) })
					p.TitleQuery = title
					hits, err := (&sqlitestore.Store{DB: raw}).SearchFTSAny(ctx, p)
					require.NoError(t, err)
					require.Equal(t, base, hits)
				})
				t.Run("purged store keeps base ranking", func(t *testing.T) {
					ctx := t.Context()
					_, err := raw.ExecContext(ctx, `DELETE FROM issues WHERE id <= 5100`)
					require.NoError(t, err)
					p := db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), Limit: 20}
					base, err := store.SearchFTSAny(ctx, p)
					require.NoError(t, err)
					capture := &beforeQueryConnector{path: path, contains: "SELECT i.id, i.title FROM issues_fts", beforeQuery: func() { t.Fatal("purged small store must skip the title walk") }}
					raw := sql.OpenDB(capture)
					t.Cleanup(func() { require.NoError(t, raw.Close()) })
					p.TitleQuery = title
					hits, err := (&sqlitestore.Store{DB: raw}).SearchFTSAny(ctx, p)
					require.NoError(t, err)
					require.Equal(t, base, hits)
				})
			}
		})
	}
}

// Title retrieval must retain scorer-equivalent titles and stay cheap on common words.
func TestSearchFTSAny_LookalikeTitlePageBudget(t *testing.T) {
	cases := []struct {
		name, title, originalTitle      string
		commonTitles, commonTitlePrefix bool
	}{
		{name: "stemmed", title: "logins callbacks drops sessions", originalTitle: "login callback drop session"},
		{name: "stopwords", title: "the and of to with login callback", originalTitle: "login callback"},
		{name: "punctuation", title: "login-callback drops-session", originalTitle: "session drop callback login"},
		{name: "rare title prefixes", title: "response token login", originalTitle: "response token login", commonTitlePrefix: true},
		{name: "common title words", title: "request response server client token account", originalTitle: "request response server client token account", commonTitles: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, path := openTestDBWithPath(t)
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			title := tc.title
			body, originalBody, distractors := dbtest.RewrittenBodyFixture(title, 5_100)
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			raw.SetMaxOpenConns(1)
			tx, err := raw.BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			insertIssue := prepareSeed(t, tx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`)
			n := 0
			for distractorTitle, distractorBody := range distractors {
				if tc.commonTitlePrefix {
					distractorTitle = "response unrelated issue"
				}
				if tc.commonTitles {
					distractorTitle = title + " alpha beta gamma delta epsilon zeta theta iota"
				}
				_, err = insertIssue.ExecContext(ctx, fmt.Sprintf("%026d", n+1), project.ID, fmt.Sprintf("%08d", n+1), distractorTitle, distractorBody)
				require.NoError(t, err)
				n++
			}
			require.NoError(t, tx.Commit())
			original := createTesterIssueWithBody(ctx, t, store, project.ID, tc.originalTitle, originalBody)
			require.GreaterOrEqual(t, similarity.Score(title, body, original.Title, original.Body), 0.7)
			searchStore := &sqlitestore.Store{DB: raw}
			params := db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), Limit: 20}
			baseline, err := searchStore.SearchFTSAny(ctx, params)
			require.NoError(t, err)
			var baselineFound bool
			for _, hit := range baseline {
				baselineFound = baselineFound || hit.Issue.ID == original.ID
			}
			require.True(t, baselineFound, "the full query must warn about the original")
			if tc.commonTitles {
				capture := &beforeQueryConnector{path: path, contains: "SELECT i.id, i.title FROM issues_fts", beforeQuery: func() { t.Fatal("capped title words must skip the title walk") }}
				hooked := sql.OpenDB(capture)
				t.Cleanup(func() { require.NoError(t, hooked.Close()) })
				params.TitleQuery = title
				params.TitleBody = body
				hits, err := (&sqlitestore.Store{DB: hooked}).SearchFTSAny(ctx, params)
				require.NoError(t, err)
				require.Equal(t, baseline, hits)
				return
			}
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			started := time.Now()
			hits, err := searchStore.SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), TitleQuery: title, TitleBody: body, Limit: 20})
			require.NoError(t, err)
			require.Len(t, hits, 1, "only titles that can reach the duplicate threshold need hydration")
			elapsed := time.Since(started)
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			count := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			require.Equal(t, original.ID, hits[0].Issue.ID, "the similar-title duplicate must survive the title lookup")
			t.Logf("rewritten body: %d page fetches, %.3f ms", count, float64(elapsed)/float64(time.Millisecond))
			budget := 6_000
			if tc.commonTitlePrefix {
				budget = 900
			}
			require.Less(t, count, budget, "common postings must be counted without scoring every document")

			if tc.name != "stemmed" {
				return
			}
			t.Run("many matching titles keep the full ranking", func(t *testing.T) {
				ctx := t.Context()
				tx, err := raw.BeginTx(ctx, nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = tx.Rollback() })
				insertIssue := prepareSeed(t, tx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`)
				for n := range 201 {
					_, err = insertIssue.ExecContext(ctx, fmt.Sprintf("%026d", n+10_000), project.ID, fmt.Sprintf("%08d", n+10_000), title, originalBody)
					require.NoError(t, err)
				}
				require.NoError(t, tx.Commit())
				params := db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), Limit: 20}
				full, err := searchStore.SearchFTSAny(ctx, params)
				require.NoError(t, err)
				params.TitleQuery = title
				params.TitleBody = body
				reduced, err := searchStore.SearchFTSAny(ctx, params)
				require.NoError(t, err)
				require.Equal(t, full, reduced)
			})
		})
	}
}

// Large-store title filtering must still find titles made only of stop words
// and one-letter words, which leave the similarity tokenizer nothing to match.
func TestSearchFTSAny_LookalikeStopWordTitle(t *testing.T) {
	for _, title := range []string{"the", "This is it", "A B"} {
		t.Run(title, func(t *testing.T) {
			ctx := t.Context()
			store, path := openTestDBWithPath(t)
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			tx, err := raw.BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			insertIssue := prepareSeed(t, tx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`)
			for n := range 5_100 {
				_, err = insertIssue.ExecContext(ctx, fmt.Sprintf("%026d", n+1), project.ID, fmt.Sprintf("%08d", n+1), "unrelated issue", "other content")
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			original := createTesterIssueWithBody(ctx, t, store, project.ID, title, "")

			hits, err := store.SearchFTSAny(ctx, db.SearchFTSParams{
				ProjectID: project.ID, Query: similarity.LookalikeQuery(title, ""),
				TitleQuery: similarity.TitlePrefix(title), Limit: 20,
			})

			require.NoError(t, err)
			require.NotEmpty(t, hits)
			require.Equal(t, original.ID, hits[0].Issue.ID)
		})
	}
}
