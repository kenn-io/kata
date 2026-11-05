package githubsync

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunnerCommentRequestCounts(t *testing.T) {
	var idleTotals []int
	for _, size := range []struct {
		k, firstPages, imported int
	}{{1, 2, 117}, {10, 94, 9270}} {
		t.Run(fmt.Sprintf("k=%d", size.k), func(t *testing.T) {
			h := newRunnerHarness(t)
			old := h.now.Add(-time.Hour)
			var issues []Issue
			type commentRow struct {
				Comment
				UpdatedAt time.Time `json:"updated_at"`
			}
			var comments []commentRow
			for number := 1; number <= 30*size.k+1; number++ {
				issue := testIssue(int64(100+number), number, fmt.Sprintf("issue %d", number), old)
				issue.Comments = 3 * size.k
				if number == 1 {
					issue.Comments = 30 * size.k
				}
				if number == 30*size.k+1 {
					issue.PullRequest = &PullRequest{}
					issue.Comments = 5 * size.k
				}
				issues = append(issues, issue)
				for range issue.Comments {
					id := int64(len(comments) + 1)
					comments = append(comments, commentRow{
						ID: id, NodeID: fmt.Sprintf("C_%d", id), Body: fmt.Sprintf("comment %d", id), User: &User{Login: "commenter"}, CreatedAt: new(old),
						IssueURL: fmt.Sprintf("https://api.github.com/repos/example-owner/example-repo/issues/%d", number), UpdatedAt: old,
					})
				}
			}
			counts := map[string]int{}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimPrefix(r.URL.Path, "/repos/example-owner/example-repo")
				counts[path]++
				if path == "/graphql" {
					writeParentGraphQLTestResponse(t, w, `{"errors":[{"type":"undefinedField","path":["query","repository","issues","nodes","parent"],"message":"unsupported parent"}]}`)
					return
				}
				if path == "" {
					writeHTTPFetcherTestResponse(t, w, `{"node_id":"R_example_repo","id":101,"full_name":"example-owner/example-repo"}`)
					return
				}
				var since time.Time
				if raw := r.URL.Query().Get("since"); raw != "" {
					var err error
					since, err = time.Parse(time.RFC3339, raw)
					require.NoError(t, err)
				}
				var rows []any
				switch {
				case path == "/issues":
					for _, issue := range issues {
						if !issue.UpdatedAt.Before(since) {
							rows = append(rows, issue)
						}
					}
				case strings.HasSuffix(path, "/comments"):
					for _, comment := range comments {
						if path == "/issues/comments" && !comment.UpdatedAt.Before(since) || path == strings.TrimPrefix(comment.IssueURL, "https://api.github.com/repos/example-owner/example-repo")+"/comments" {
							rows = append(rows, comment)
						}
					}
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
					return
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page == 0 {
					page = 1
				}
				start := min((page-1)*100, len(rows))
				end := min(start+100, len(rows))
				if end < len(rows) {
					next := *r.URL
					query := next.Query()
					query.Set("page", strconv.Itoa(page+1))
					next.RawQuery = query.Encode()
					w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, server.URL, next.String()))
				}
				require.NoError(t, json.MarshalWrite(w, rows[start:end]))
			}))
			defer server.Close()
			h.runner.config.Fetcher = NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL, GraphQLURLOverride: server.URL + "/graphql"})
			for _, stage := range []string{"first", "idle", "retitle", "new comment"} {
				clear(counts)
				if stage != "first" {
					h.advance(5 * time.Minute)
				}
				if stage == "retitle" {
					issues[0].Title = "renamed"
					issues[0].UpdatedAt = new(h.now)
				}
				if stage == "new comment" {
					issues[0].UpdatedAt = new(h.now)
					issues[0].Comments++
					comments = append(comments, commentRow{ID: 99999, NodeID: "C_new", Body: "new comment", User: &User{Login: "commenter"}, CreatedAt: new(h.now), IssueURL: comments[0].IssueURL, UpdatedAt: h.now})
				}
				result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
				require.NoError(t, err)
				commentRequests, total := 0, 0
				for path, count := range counts {
					total += count
					if strings.HasSuffix(path, "/comments") {
						commentRequests += count
					}
				}
				t.Logf("%s: comment requests=%d, total requests=%d", stage, commentRequests, total)
				switch stage {
				case "first":
					assert.Equal(t, size.firstPages, commentRequests)
					assert.Equal(t, 30*size.k, result.Import.Created)
					assert.Equal(t, size.imported, result.Import.Comments)
				case "idle":
					assert.Zero(t, commentRequests)
					idleTotals = append(idleTotals, total)
				case "retitle":
					assert.Equal(t, 1, commentRequests)
					assert.Zero(t, result.Import.Comments)
				case "new comment":
					assert.Equal(t, 1, commentRequests)
					assert.Equal(t, 1, result.Import.Comments)
				}
			}
		})
	}
	require.Len(t, idleTotals, 2)
	assert.Equal(t, idleTotals[0], idleTotals[1])
}
