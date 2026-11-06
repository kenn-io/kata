package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
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
			for n := range 2_000 {
				_, err = tx.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`, fmt.Sprintf("%026d", n+1), project.ID, fmt.Sprintf("%08d", n+1), tc.title, tc.body)
				require.NoError(t, err)
			}
			for n := range tc.commentCount {
				start := n % (len(words) - 16)
				_, err = tx.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body) VALUES(?,(SELECT id FROM issues WHERE project_id=? ORDER BY id LIMIT 1 OFFSET ?),'tester',?)`, fmt.Sprintf("%026d", n+1), project.ID, n%2_000, strings.Join(words[start:start+16], " "))
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

// Title retrieval must retain scorer-equivalent titles and stay cheap on common words.
func TestSearchFTSAny_LookalikeTitlePageBudget(t *testing.T) {
	cases := []struct {
		name, title, originalTitle      string
		matchedIn                       []string
		commonTitles, commonTitlePrefix bool
	}{
		{name: "stemmed", title: "logins callbacks drops sessions", originalTitle: "login callback drop session", matchedIn: []string{"body"}},
		{name: "stopwords", title: "the and of to with login callback", originalTitle: "login callback", matchedIn: []string{"title", "body"}},
		{name: "punctuation", title: "login-callback drops-session", originalTitle: "session drop callback login", matchedIn: []string{"body"}},
		{name: "rare title prefixes", title: "response token login", originalTitle: "response token login", matchedIn: []string{"title", "body"}, commonTitlePrefix: true},
		{name: "common title words", title: "request response server client token account", originalTitle: "request response server client token account", matchedIn: []string{"title", "body"}, commonTitles: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, path := openTestDBWithPath(t)
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			title := tc.title
			const common = "request response server client token account"
			const rare = "amber birch cedar delta elm fir grove hazel"
			const retained = "iris juniper maple oak"
			const body = common + " " + rare + " " + retained
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			raw.SetMaxOpenConns(1)
			tx, err := raw.BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			for n := range 5_100 {
				distractorTitle, distractorBody := "unrelated issue", common
				words := strings.Fields(title)
				if n < len(words)*100 {
					distractorTitle = words[n/100] + " unrelated issue"
				}
				if tc.commonTitlePrefix {
					distractorTitle = "response unrelated issue"
				}
				if tc.commonTitles {
					distractorTitle = title + " alpha beta gamma delta epsilon zeta theta iota"
				}
				if n >= 400 && n < 424 {
					distractorBody += " " + rare + " " + strings.Repeat("padding ", 120)
				}
				if n >= 800 && n < 900 {
					distractorBody += " " + retained + " " + strings.Repeat("padding ", 120)
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`, fmt.Sprintf("%026d", n+1), project.ID, fmt.Sprintf("%08d", n+1), distractorTitle, distractorBody)
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			original := createTesterIssueWithBody(ctx, t, store, project.ID, tc.originalTitle, common+" "+retained+" rewritten narrative")
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
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			started := time.Now()
			hits, err := searchStore.SearchFTSAny(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), TitleQuery: title, Limit: 20})
			require.NoError(t, err)
			require.Equal(t, 1, len(hits), "only titles that can reach the duplicate threshold need hydration")
			elapsed := time.Since(started)
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			count := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			var found bool
			for _, hit := range hits {
				if hit.Issue.ID == original.ID {
					found = true
					require.Equal(t, tc.matchedIn, hit.MatchedIn)
				}
			}
			t.Logf("rewritten body: %d page fetches, %.3f ms", count, float64(elapsed)/float64(time.Millisecond))
			require.True(t, found, "the similar-title duplicate must survive the title lookup")
			budget := 6_000
			if tc.commonTitlePrefix {
				budget = 900
			}
			require.Less(t, count, budget, "common postings must be counted without scoring every document")

			t.Run("many matching titles keep the full ranking", func(t *testing.T) {
				ctx := t.Context()
				tx, err := raw.BeginTx(ctx, nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = tx.Rollback() })
				for n := range 201 {
					_, err = tx.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,body,author) VALUES(?,?,?,?,?,'tester')`, fmt.Sprintf("%026d", n+10_000), project.ID, fmt.Sprintf("%08d", n+10_000), title, common)
					require.NoError(t, err)
				}
				require.NoError(t, tx.Commit())
				params := db.SearchFTSParams{ProjectID: project.ID, Query: similarity.LookalikeQuery(title, body), Limit: 20}
				full, err := searchStore.SearchFTSAny(ctx, params)
				require.NoError(t, err)
				params.TitleQuery = title
				reduced, err := searchStore.SearchFTSAny(ctx, params)
				require.NoError(t, err)
				require.Equal(t, full, reduced)
			})
		})
	}
}
