package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
)

const attachmentID = "01JB3Z6V2K8M4Q9T1X5W7Y0RNA"

func attachmentEnv(t *testing.T, maxBytes int64) (*testenv.Env, *docbanktest.Fake) {
	t.Helper()
	f := docbanktest.New(t)
	client, err := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Docbank = client
		cfg.DocbankFolder = "/kata"
		cfg.MaxAttachmentBytes = maxBytes
	})
	return env, f
}
func attachmentUpload(t *testing.T, env *testenv.Env, p string, id string, data []byte, count int, token ...string) (int, []byte) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	require.NoError(t, w.WriteField("attachment_id", id))
	require.NoError(t, w.WriteField("actor", "operator"))
	for range count {
		f, err := w.CreateFormFile("file", "screen.png")
		require.NoError(t, err)
		_, err = f.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	req, err := http.NewRequest("POST", env.URL+p, &b)
	require.NoError(t, err)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if len(token) > 0 {
		req.Header.Set("Authorization", "Bearer "+token[0])
	}
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}
func TestAttachmentAddRetryConflictAndRemove(t *testing.T) {
	env, f := attachmentEnv(t, 1024)
	p, iss := seedProjectAndIssue(t, env)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
	code, raw := attachmentUpload(t, env, path, attachmentID, []byte("plain text"), 1)
	require.Equal(t, 200, code, string(raw))
	var out struct {
		Attachment metadata.Attachment `json:"attachment"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "text/plain; charset=utf-8", out.Attachment.MediaType)
	require.Equal(t, "screen.png", out.Attachment.Name)
	before, err := env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	code, raw = attachmentUpload(t, env, path, attachmentID, []byte("plain text"), 1)
	require.Equal(t, 200, code, string(raw))
	after, err := env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	code, raw = attachmentUpload(t, env, path, attachmentID, []byte("different"), 1)
	require.Equal(t, 409, code, string(raw))
	require.Contains(t, string(raw), "attachment_id_conflict")
	calls := len(f.Requests())
	req, _ := http.NewRequest("DELETE", env.URL+path+"/"+attachmentID+"?actor=operator", nil)
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, f.Requests(), calls)
	after, err = env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	require.Contains(t, string(after.Metadata), `"attachments":{}`)
	resp, err = env.HTTP.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 404, resp.StatusCode)
}
func TestAttachmentRejectsBeforeDocbank(t *testing.T) {
	for _, tc := range []struct {
		name              string
		size, count, code int
	}{{"too large", 33, 1, 413}, {"no file", 0, 0, 400}, {"two files", 1, 2, 400}, {"envelope", (1 << 20) + 1, 1, 413}} {
		t.Run(tc.name, func(t *testing.T) {
			env, f := attachmentEnv(t, 32)
			p, iss := seedProjectAndIssue(t, env)
			path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
			code, raw := attachmentUpload(t, env, path, attachmentID, bytes.Repeat([]byte("a"), tc.size), tc.count)
			require.Equal(t, tc.code, code, string(raw))
			require.Empty(t, f.Requests())
		})
	}
	env := testenv.New(t)
	p, iss := seedProjectAndIssue(t, env)
	code, raw := attachmentUpload(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID), attachmentID, []byte("x"), 1)
	require.Equal(t, 503, code, string(raw))
	require.Contains(t, string(raw), "docbank_not_configured")
}
func TestAttachmentUpstreamErrorDoesNotWrite(t *testing.T) {
	env, f := attachmentEnv(t, 1024)
	p, iss := seedProjectAndIssue(t, env)
	f.Fault = "status:503"
	code, raw := attachmentUpload(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID), attachmentID, []byte("x"), 1)
	require.Equal(t, 502, code, string(raw))
	after, err := env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	require.Equal(t, iss.Revision, after.Revision)
	require.NotContains(t, string(after.Metadata), "attachments")
}
func TestAttachmentUnknownIssueDoesNotCallDocbank(t *testing.T) {
	env, f := attachmentEnv(t, 1024)
	p, _ := seedProjectAndIssue(t, env)
	code, _ := attachmentUpload(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/unknown/attachments", p.ID), attachmentID, []byte(strings.Repeat("x", 3)), 1)
	require.Equal(t, 404, code)
	require.Empty(t, f.Requests())
}

// A real competing metadata write forces the handler to re-read and retain the peer entry.
type attachmentConflictStore struct {
	db.Storage
	once sync.Once
}

func (s *attachmentConflictStore) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	s.once.Do(func() {
		_, _ = s.Storage.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: in.IssueID, Actor: "operator", Patch: map[string]jsontext.Value{"attachments": jsontext.Value(`{"01JB3Z6V2K8M4Q9T1X5W7Y0RNB":{"future":true}}`)}})
	})
	return s.Storage.PatchIssueMetadata(ctx, in)
}
func TestAttachmentGuardRetryPreservesPeer(t *testing.T) {
	f := docbanktest.New(t)
	client, err := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Docbank = client
		cfg.DB = &attachmentConflictStore{Storage: cfg.DB}
	})
	p, iss := seedProjectAndIssue(t, env)
	code, raw := attachmentUpload(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID), attachmentID, []byte("x"), 1)
	require.Equal(t, 200, code, string(raw))
	after, err := env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	require.Contains(t, string(after.Metadata), `"future":true`)
	require.Contains(t, string(after.Metadata), attachmentID)
}

type attachmentMoveBeforePatchStore struct {
	db.Storage
	fromProjectID int64
	toProjectID   int64
}

func (s *attachmentMoveBeforePatchStore) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	issue, err := s.IssueByID(ctx, in.IssueID)
	if err != nil {
		return db.PatchIssueMetadataOut{}, err
	}
	if issue.ProjectID == s.fromProjectID {
		if _, err := s.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: issue.ID, FromProjectID: s.fromProjectID, ToProjectID: s.toProjectID,
			IfMatchRev: issue.Revision, Actor: "operator",
		}); err != nil {
			return db.PatchIssueMetadataOut{}, err
		}
	}
	return s.Storage.PatchIssueMetadata(ctx, in)
}

func TestAttachmentPublishUsesDestinationAfterConcurrentProjectMove(t *testing.T) {
	f := docbanktest.New(t)
	client, err := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
	store := &attachmentMoveBeforePatchStore{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		store.Storage = cfg.DB
		cfg.DB = store
		cfg.Docbank = client
	})
	source, issue := seedProjectAndIssue(t, env)
	destination, err := env.DB.CreateProject(t.Context(), "destination-project")
	require.NoError(t, err)
	store.fromProjectID = source.ID
	store.toProjectID = destination.ID
	sourceSub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: source.ID})
	defer sourceSub.Unsub()
	destinationSub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: destination.ID})
	defer destinationSub.Unsub()

	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", source.ID, issue.ShortID)
	code, raw := attachmentUpload(t, env, path, attachmentID, []byte("moved with attachment"), 1)
	require.Equal(t, http.StatusOK, code, string(raw))

	stored, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, destination.ID, stored.ProjectID)
	msg := receiveMsg(t, destinationSub.Ch, time.Second, "attachment event in destination project")
	require.NotNil(t, msg.Event)
	require.Equal(t, destination.ID, msg.ProjectID)
	require.Equal(t, destination.ID, msg.Event.ProjectID)
	select {
	case msg := <-sourceSub.Ch:
		t.Fatalf("attachment event was published to source project: %+v", msg)
	default:
	}
}

func TestAttachmentScopeBeforeOutbound(t *testing.T) {
	f := docbanktest.New(t)
	c, err := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity(), func(cfg *daemon.ServerConfig) { cfg.Docbank = c })
	p, root := seedProjectAndIssue(t, env)
	hidden := createScopedHTTPTestIssue(t, env, p.ID, "Hidden", nil)
	expires := time.Now().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "worker-token", Actor: "operator", AdminActor: db.BootstrapActor, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: p.UID, RootIssueUID: root.UID}, ExpiresAt: &expires})
	require.NoError(t, err)
	code, raw := attachmentUpload(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, hidden.ShortID), attachmentID, []byte("x"), 1, "worker-token")
	require.Equal(t, 404, code, string(raw))
	require.Empty(t, f.Requests())
}
func TestAttachmentLimitKeepsInvalidPeersRemovable(t *testing.T) {
	env, f := attachmentEnv(t, 1024)
	p, iss := seedProjectAndIssue(t, env)
	m := metadata.Attachments{}
	for i := range 100 {
		m[fmt.Sprintf("%026d", i)] = jsontext.Value(`{"future":true}`)
	}
	_, err := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachments": m.Encode()}})
	require.NoError(t, err)
	code, raw := attachmentUpload(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID), attachmentID, []byte("x"), 1)
	require.Equal(t, 400, code, string(raw))
	require.Contains(t, string(raw), "attachment_invalid")
	require.Empty(t, f.Requests(), "an issue at the attachment limit is rejected before Docbank is contacted")
}

func TestAttachmentMalformedMapRejectedBeforeDocbank(t *testing.T) {
	f := docbanktest.New(t)
	client, err := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
	store := &attachmentMalformedMetadataStore{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Docbank = client
		store.Storage = cfg.DB
		cfg.DB = store
	})
	p, iss := seedProjectAndIssue(t, env)
	code, raw := attachmentUpload(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID), attachmentID, []byte("x"), 1)
	require.Equal(t, 400, code, string(raw))
	require.Contains(t, string(raw), "attachment_invalid")
	require.Empty(t, f.Requests(), "a malformed attachment map is rejected before Docbank is contacted")
}

type attachmentMalformedMetadataStore struct{ db.Storage }

func (s *attachmentMalformedMetadataStore) IssueByShortID(ctx context.Context, projectID int64, shortID string, include db.IncludeDeleted) (db.Issue, error) {
	issue, err := s.Storage.IssueByShortID(ctx, projectID, shortID, include)
	if err == nil {
		issue.Metadata = db.JSONBlob(`{"attachments":[]}`)
	}
	return issue, err
}

type attachmentBlockingStore struct {
	db.Storage
	entered chan struct{}
	release chan struct{}
}

func (s *attachmentBlockingStore) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return db.PatchIssueMetadataOut{}, ctx.Err()
	}
	return s.Storage.PatchIssueMetadata(ctx, in)
}
func TestAttachmentConcurrentUploadBound(t *testing.T) {
	f := docbanktest.New(t)
	c, err := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Docbank = c
		cfg.DB = &attachmentBlockingStore{cfg.DB, entered, release}
	})
	p, iss := seedProjectAndIssue(t, env)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
	done := make(chan int, 2)
	for i := range 2 {
		go func() {
			code, _ := attachmentUpload(t, env, path, fmt.Sprintf("%026d", i), []byte("x"), 1)
			done <- code
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatal("uploads did not reach metadata write")
		}
	}
	code, _ := attachmentUpload(t, env, path, attachmentID, []byte("x"), 1)
	close(release)
	require.Equal(t, 429, code)
	for range 2 {
		require.Equal(t, 200, <-done)
	}
}

func TestAttachmentContentAndText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   []byte
		inline bool
	}{
		{"PNG", append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 600)...), true},
		{"SVG", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), false},
		{"HTML", []byte(`<html>hello</html>`), false},
		{"video", []byte("\x00\x00\x00\x18ftypmp42video"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, f := attachmentEnv(t, 1024)
			p, iss := seedProjectAndIssue(t, env)
			base := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
			code, raw := attachmentUpload(t, env, base, attachmentID, tc.data, 1)
			require.Equal(t, 200, code, string(raw))
			resp, err := env.HTTP.Get(env.URL + base + "/" + attachmentID + "/content")
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, 200, resp.StatusCode, string(b))
			require.Equal(t, tc.data, b)
			require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
			require.Equal(t, "sandbox", resp.Header.Get("Content-Security-Policy"))
			require.Equal(t, int64(len(tc.data)), resp.ContentLength)
			require.Empty(t, resp.Header.Get("X-Docbank-Blob-Hash"))
			if tc.inline {
				require.Equal(t, "image/png", resp.Header.Get("Content-Type"))
			} else {
				require.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
				require.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")
			}
			f.State = "ready"
			f.Text = "café text"
			resp, err = env.HTTP.Get(env.URL + base + "/" + attachmentID + "/text?max_bytes=4")
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			b, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, 200, resp.StatusCode, string(b))
			require.Contains(t, string(b), `"text":"caf"`)
			require.Contains(t, string(b), `"truncated":true`)
		})
	}
}
func TestAttachmentReadErrors(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		code  int
		state string
	}{{"foreign", 409, "foreign_vault"}, {"invalid", 400, "attachment_invalid"}, {"missing", 410, "attachment_content_missing"}, {"unknown", 404, "attachment_not_found"}} {
		t.Run(tc.kind, func(t *testing.T) {
			env, f := attachmentEnv(t, 1024)
			p, iss := seedProjectAndIssue(t, env)
			base := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
			code, raw := attachmentUpload(t, env, base, attachmentID, []byte("hello"), 1)
			require.Equal(t, 200, code, string(raw))
			current, err := env.DB.IssueByID(t.Context(), iss.ID)
			require.NoError(t, err)
			var out struct {
				Attachment metadata.Attachment `json:"attachment"`
			}
			require.NoError(t, json.Unmarshal(raw, &out))
			e := out.Attachment
			id := attachmentID
			switch tc.kind {
			case "foreign":
				e.Docbank.VaultUID = "7f1c2e8a-1111-4111-8111-111111111111"
			case "invalid":
				e.Docbank.NodeID = 0
			case "missing":
				f.Fault = "status:404"
			case "unknown":
				id = "unknown"
			}
			_ = current
			b, err := json.Marshal(e)
			require.NoError(t, err)
			_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachments": metadata.Attachments{attachmentID: b}.Encode()}})
			require.NoError(t, err)
			calls := len(f.Requests())
			for _, route := range []string{"content", "text"} {
				resp, err := env.HTTP.Get(env.URL + base + "/" + id + "/" + route)
				require.NoError(t, err)
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				require.Equal(t, tc.code, resp.StatusCode, string(b))
				require.Contains(t, string(b), tc.state)
			}
			if tc.kind == "foreign" {
				require.Len(t, f.Requests(), calls+2)
			} // identity only, never content
			if tc.kind == "invalid" || tc.kind == "unknown" {
				require.Len(t, f.Requests(), calls)
			}
		})
	}
}
func TestAttachmentIntegrityAbortsContent(t *testing.T) {
	env, f := attachmentEnv(t, 1<<20)
	p, iss := seedProjectAndIssue(t, env)
	base := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
	code, raw := attachmentUpload(t, env, base, attachmentID, bytes.Repeat([]byte("x"), 100000), 1)
	require.Equal(t, 200, code, string(raw))
	f.Fault = "trailer"
	resp, err := env.HTTP.Get(env.URL + base + "/" + attachmentID + "/content")
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		_, err = io.ReadAll(resp.Body)
	}
	require.Error(t, err)
}

type attachmentPostCommitScopeFailureStore struct {
	db.Storage
	committed db.Event
}

func (s *attachmentPostCommitScopeFailureStore) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	result, err := s.Storage.PatchIssueMetadata(ctx, in)
	if err == nil && result.Changed {
		s.committed = result.Event
	}
	return result, err
}
func (s *attachmentPostCommitScopeFailureStore) IssueScopedMembers(ctx context.Context, scope db.APITokenScope) ([]db.Issue, error) {
	if s.committed.ID != 0 {
		return nil, errors.New("response-only scope lookup failed")
	}
	return s.Storage.IssueScopedMembers(ctx, scope)
}
func TestAttachmentPublishesBeforeResponseScopeLookup(t *testing.T) {
	f := docbanktest.New(t)
	client, err := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
	wrapped := &attachmentPostCommitScopeFailureStore{}
	hooks := &publisherSink{}
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity(), func(cfg *daemon.ServerConfig) {
		wrapped.Storage = cfg.DB
		cfg.DB = wrapped
		cfg.Docbank = client
		cfg.Hooks = hooks
	})
	p, iss := seedProjectAndIssue(t, env)
	newScopedTokens(t, env, p, iss)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("attachment_id", attachmentID))
	require.NoError(t, writer.WriteField("actor", "worker-a"))
	part, err := writer.CreateFormFile("file", "note.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("note"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	request, err := http.NewRequest(http.MethodPost, env.URL+fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID), &body)
	require.NoError(t, err)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer worker-token")
	response, err := env.HTTP.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NotZero(t, wrapped.committed.ID)
	require.Contains(t, hooks.ids(), wrapped.committed.ID, "committed reference must reach hooks")
	msg := receiveMsg(t, sub.Ch, time.Second, "committed attachment event")
	require.NotNil(t, msg.Event)
	require.Equal(t, wrapped.committed.ID, msg.Event.ID)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
}
