package daemon_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The tests in this file set Accept-Encoding explicitly on every request so
// net/http's transparent gzip handling stays out of the way and the assertions
// hold against the actual wire bytes.

// mkLargeIssue creates an issue whose body is comfortably above any sane
// compression minimum-size threshold, so list responses containing it are
// eligible for gzip.
func mkLargeIssue(t *testing.T, env *testenv.Env, projectID int64, title string) db.Issue {
	t.Helper()
	is, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID,
		Title:     title,
		Body:      strings.Repeat("kata compresses large JSON responses over slow links. ", 100),
		Author:    "tester",
	})
	require.NoError(t, err)
	return is
}

func TestGzip_CompressesLargeJSONWhenRequested(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	mkLargeIssue(t, env, pid, "compressible")

	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/issues?status=open", nil,
		map[string]string{"Accept-Encoding": "gzip"})
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))

	zr, err := gzip.NewReader(bytes.NewReader(bs))
	require.NoError(t, err)
	plain, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Less(t, len(bs), len(plain), "compressed wire bytes must be smaller than the JSON they encode")

	var body struct {
		Issues []struct {
			Title string `json:"title"`
		} `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(plain, &body), string(plain))
	require.Len(t, body.Issues, 1)
	assert.Equal(t, "compressible", body.Issues[0].Title)
}

func TestGzip_IdentityWhenClientDoesNotAcceptGzip(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	mkLargeIssue(t, env, pid, "plain")

	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/issues?status=open", nil,
		map[string]string{"Accept-Encoding": "identity"})
	require.Equal(t, 200, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Content-Encoding"))

	var body struct {
		Issues []struct {
			Title string `json:"title"`
		} `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(bs, &body), string(bs))
	require.Len(t, body.Issues, 1)
	assert.Equal(t, "plain", body.Issues[0].Title)
}

func TestGzip_SmallResponsesStayUncompressed(t *testing.T) {
	env := testenv.New(t)

	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/ping", nil,
		map[string]string{"Accept-Encoding": "gzip"})
	require.Equal(t, 200, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Content-Encoding"),
		"tiny responses must skip gzip overhead")
	assert.Contains(t, string(bs), `"ok":true`, "body must be plain JSON")
}

func TestGzip_SSEStreamNotCompressedAndStillStreams(t *testing.T) {
	env := testenv.New(t)
	pid := mkProject(t, env, "github.com/test/a", "a")
	_, sentinelEvt := mkIssueWithEvent(t, env, pid, "sentinel")

	hwm, err := env.DB.MaxEventID(context.Background())
	require.NoError(t, err)
	resp := openSSE(t, env, "after_id="+strconv.FormatInt(hwm-1, 10), http.Header{
		"Accept":          {"text/event-stream"},
		"Accept-Encoding": {"gzip"},
	})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Content-Encoding"),
		"SSE stream must not be compressed even when the client accepts gzip")

	framer := newSSEFramer(resp.Body)
	first, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "drain frame must arrive promptly over the uncompressed stream")
	assert.Equal(t, strconv.FormatInt(sentinelEvt.ID, 10), first.id)
}
