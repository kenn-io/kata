package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/testenv"
)

const linkVersion = "6f1c2e8a-2222-4222-8222-222222222222"
const oldLinkVersion = "6f1c2e8a-2222-4222-8222-222222222223"

func seedLinkFile(f *docbanktest.Fake) {
	f.Seed("/reports/report.pdf", docbanktest.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: linkVersion},
		docbanktest.Version{ID: linkVersion, NodeID: 42, Size: 12, MediaType: "application/pdf"},
		docbanktest.Version{ID: oldLinkVersion, NodeID: 42, Size: 7})
}

func TestAttachmentLinkPinsExistingVersion(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	seedLinkFile(f)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID)
	code, raw, err := postAttachmentLink(env, endpoint, `{"path":"/reports/report.pdf","actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	var out api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, "report.pdf", out.Attachment.Name)
	require.Equal(t, linkVersion, out.Attachment.DocbankVersion.String())
	require.EqualValues(t, 12, out.Attachment.Size)
	code, raw, err = postAttachmentLink(env, endpoint, `{"node_id":42,"version_id":"`+oldLinkVersion+`","actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, oldLinkVersion, out.Attachment.DocbankVersion.String())
	require.EqualValues(t, 7, out.Attachment.Size)
	require.Equal(t, "application/octet-stream", out.Attachment.MediaType)
	for _, request := range f.Requests() {
		require.True(t, strings.HasPrefix(request, "GET "), request)
	}
}

func postAttachmentLink(env *testenv.Env, endpoint, body string) (int, []byte, error) {
	resp, err := env.HTTP.Post(env.URL+endpoint, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

func TestAttachmentLinkRejectsInvalidSelections(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind, trash, fileName, fault string
		nodeID                                   int64
		status                                   int
		code                                     string
	}{
		{name: "empty", body: `{}`, status: 400, code: "attachment_invalid"},
		{name: "both", body: `{"node_id":42,"path":"/reports/report.pdf"}`, status: 400, code: "attachment_invalid"},
		{name: "zero", body: `{"node_id":0}`, status: 400, code: "attachment_invalid"},
		{name: "negative", body: `{"node_id":-1}`, status: 400, code: "attachment_invalid"},
		{name: "relative", body: `{"path":"report.pdf"}`, status: 400, code: "attachment_invalid"},
		{name: "bad version", body: `{"node_id":42,"version_id":"bad"}`, status: 400, code: "attachment_invalid"},
		{name: "zero version", body: `{"node_id":42,"version_id":"00000000-0000-0000-0000-000000000000"}`, status: 400, code: "attachment_invalid"},
		{name: "directory", kind: "dir", status: 400, code: "attachment_not_file"},
		{name: "trashed", trash: "2026-10-25T01:00:00Z", status: 410, code: "attachment_content_missing"},
		{name: "wrong node", body: `{"node_id":42,"version_id":"` + oldLinkVersion + `"}`, nodeID: 43, status: 400, code: "attachment_version_mismatch"},
		{name: "bad name", fileName: strings.Repeat("a", 256), status: 400, code: "attachment_invalid"},
		{name: "missing", fault: "status:404", status: 410, code: "attachment_content_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, f := attachmentEnv(t)
			p, iss := seedProjectAndIssue(t, env)
			if tc.kind == "" {
				tc.kind = "file"
			}
			if tc.fileName == "" {
				tc.fileName = "report.pdf"
			}
			if tc.nodeID == 0 {
				tc.nodeID = 42
			}
			node := docbanktest.Node{ID: 42, Name: tc.fileName, Kind: tc.kind, TrashedAt: tc.trash}
			if tc.kind == "file" {
				node.CurrentVersionID = linkVersion
				f.Seed("/reports/report.pdf", node, docbanktest.Version{ID: linkVersion, NodeID: 42, Size: 12})
			} else {
				f.Seed("/reports/report.pdf", node)
			}
			if tc.nodeID != 42 {
				f.Seed("", docbanktest.Node{ID: tc.nodeID, Name: "other.pdf", Kind: "file", CurrentVersionID: oldLinkVersion}, docbanktest.Version{ID: oldLinkVersion, NodeID: tc.nodeID, Size: 7})
			}
			f.Fault = tc.fault
			body := tc.body
			if body == "" {
				body = `{"node_id":42}`
			}
			var input map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &input))
			input["actor"] = "operator"
			rawInput, err := json.Marshal(input)
			require.NoError(t, err)
			status, raw, err := postAttachmentLink(env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID), string(rawInput))
			require.NoError(t, err)
			assertAPIError(t, status, raw, tc.status, tc.code)
			stored, err := env.DB.IssueByID(t.Context(), iss.ID)
			require.NoError(t, err)
			require.JSONEq(t, `{}`, string(stored.Metadata))
			if tc.body != "" && tc.nodeID == 42 {
				require.Empty(t, f.Requests(), "invalid selector must fail before upstream reads")
			}
		})
	}
}

func TestAttachmentLinkConcurrentRequestsConverge(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	seedLinkFile(f)
	checkConcurrentAttachmentLinks(t, env, env.DB, p, iss)
}

func TestAttachmentLinkConcurrentPostgresRequestsConverge(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	p, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	iss, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Link report", Author: "operator"})
	require.NoError(t, err)
	f := docbanktest.New(t)
	seedLinkFile(f)
	broadcaster := daemon.NewEventBroadcaster()
	d := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC(), Broadcaster: broadcaster, Docbank: docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })})
	t.Cleanup(func() { _ = d.Close() })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	env := &testenv.Env{URL: server.URL, HTTP: server.Client(), Broadcaster: broadcaster}
	checkConcurrentAttachmentLinks(t, env, store, p, iss)
}

func checkConcurrentAttachmentLinks(t *testing.T, env *testenv.Env, store db.Storage, p db.Project, iss db.Issue) {
	t.Helper()
	before, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID)
	const workers = 8
	type result struct {
		status int
		raw    []byte
		err    error
	}
	done := make(chan result, workers)
	for range workers {
		go func() {
			status, raw, err := postAttachmentLink(env, endpoint, `{"node_id":42,"actor":"operator"}`)
			done <- result{status, raw, err}
		}()
	}
	id := ""
	for range workers {
		result := <-done
		require.NoError(t, result.err)
		require.Equal(t, 200, result.status, string(result.raw))
		var out api.AddAttachmentResponseBody
		require.NoError(t, json.Unmarshal(result.raw, &out))
		if id == "" {
			id = out.ID
		}
		require.Equal(t, id, out.ID)
	}
	stored, err := store.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	require.Equal(t, iss.Revision+1, stored.Revision)
	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(stored.Metadata), &meta))
	require.Len(t, meta, 1)
	events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{AfterID: before, ProjectID: p.ID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, events, 1, "exactly one committed mutation event")
	message := receiveMsg(t, sub.Ch, time.Second, "link event")
	require.Equal(t, events[0].UID, message.Event.UID)
	assertNoReceive(t, sub.Ch, 20*time.Millisecond, "duplicate link event")
}

func TestAttachmentLinkRechecksAuthorityAfterLookup(t *testing.T) {
	f := docbanktest.New(t)
	seedLinkFile(f)
	reached, resume := make(chan struct{}), make(chan struct{})
	started := sync.OnceFunc(func() { close(reached) })
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/versions/") {
			started()
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		f.Server.Config.Handler.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Docbank = docbank.NewClient(upstream.URL, func() (string, error) { return "test-key", nil })
	})
	p, iss, _ := setupClaimGateProject(t, env, true)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID)
	type result struct {
		status int
		raw    []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, raw, err := postAttachmentLink(env, endpoint, `{"node_id":42,"actor":"operator"}`)
		done <- result{status, raw, err}
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup did not reach Docbank")
	}
	acquireClaimGateIssue(t, env, p, iss, "other")
	release()
	out := <-done
	require.NoError(t, out.err)
	assertAPIError(t, out.status, out.raw, 409, "claim_denied")
	stored, err := env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	require.Equal(t, iss.Metadata, stored.Metadata)
}
