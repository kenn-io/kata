package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func attachmentEnv(t *testing.T) (*testenv.Env, *docbanktest.Fake) {
	t.Helper()
	f := docbanktest.New(t)
	client := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	return testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Docbank = client; cfg.DocbankFolder = "/kata" }), f
}
func attachmentUpload(t *testing.T, env *testenv.Env, endpoint, name string, data io.Reader, token ...string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", env.URL+endpoint+"?name="+name+"&actor=operator", data)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/octet-stream")
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
func TestAttachmentRawBodyReference(t *testing.T) {
	env, _ := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	var ids []string
	for range 2 {
		code, raw := attachmentUpload(t, env, endpoint, "shot.png", strings.NewReader("image bytes"))
		require.Equal(t, 200, code, string(raw))
		var out struct {
			ID string `json:"attachment_id"`
		}
		require.NoError(t, json.Unmarshal(raw, &out))
		require.True(t, uid.Valid(out.ID))
		ids = append(ids, out.ID)
		msg := receiveMsg(t, sub.Ch, time.Second, "attachment metadata event")
		require.Equal(t, "operator", msg.Event.Actor)
		var payload struct {
			Diff map[string]json.RawMessage `json:"diff"`
		}
		require.NoError(t, json.Unmarshal([]byte(msg.Event.Payload), &payload))
		require.Len(t, payload.Diff, 1)
		require.Contains(t, payload.Diff, "attachment."+out.ID)
	}
	require.NotEqual(t, ids[0], ids[1])
	stored, err := env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	var values map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(stored.Metadata), &values))
	require.Len(t, values, 2)
	require.Contains(t, values, "attachment."+ids[0])
}

func TestAttachmentUploadRechecksAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claim  bool
		status int
		code   string
	}{
		{name: "claim taken", claim: true, status: http.StatusConflict, code: "claim_denied"},
		{name: "project archived", status: http.StatusNotFound, code: "project_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := docbanktest.New(t)
			uploadStarted, resumeUpload := make(chan struct{}), make(chan struct{})
			started := sync.OnceFunc(func() { close(uploadStarted) })
			resume := sync.OnceFunc(func() { close(resumeUpload) })
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/api/v1/uploads" {
					started()
					select {
					case <-resumeUpload:
					case <-r.Context().Done():
						return
					}
				}
				f.Server.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(upstream.Close)
			defer resume()
			env := testenv.New(t, func(cfg *daemon.ServerConfig) {
				cfg.Docbank = docbank.NewClient(upstream.URL, func() (string, error) { return "test-key", nil })
				cfg.DocbankFolder = "/kata"
			})
			var p db.Project
			var iss db.Issue
			if tc.claim {
				p, iss, _ = setupClaimGateProject(t, env, true)
			} else {
				p, iss = seedProjectAndIssue(t, env)
			}
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+endpoint+"?name=shot.png&actor=operator", strings.NewReader("image bytes"))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			type result struct {
				resp *http.Response
				err  error
			}
			completed := make(chan result, 1)
			go func() {
				resp, err := env.HTTP.Do(req)
				completed <- result{resp, err}
			}()
			select {
			case <-uploadStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("upload did not reach Docbank")
			}
			if tc.claim {
				acquireClaimGateIssue(t, env, p, iss, "other")
			} else {
				_, _, err = env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{
					ProjectID: p.ID, Actor: "tester", Force: true,
				})
				require.NoError(t, err)
			}
			resume()
			out := <-completed
			require.NoError(t, out.err)
			defer func() { _ = out.resp.Body.Close() }()
			raw, err := io.ReadAll(out.resp.Body)
			require.NoError(t, err)

			// A new request exercises the early gate after authority changes.
			requestsBefore := f.Requests()
			code, earlyRaw := attachmentUpload(t, env, endpoint, "shot.png", strings.NewReader("image bytes"))
			assertAPIError(t, code, earlyRaw, tc.status, tc.code)
			assert.Equal(t, requestsBefore, f.Requests(), "the early gate must reject before another vault upload")
			stored, err := env.DB.IssueByID(t.Context(), iss.ID)
			require.NoError(t, err)
			assert.Equal(t, string(iss.Metadata), string(stored.Metadata), "upload must leave attachment metadata unchanged")
			assertAPIError(t, out.resp.StatusCode, raw, tc.status, tc.code)
		})
	}
}

type attachmentUnreadBody struct{ reads int }

func (b *attachmentUnreadBody) Read([]byte) (int, error) { b.reads++; return 0, io.ErrUnexpectedEOF }
func TestAttachmentAdmissionBeforeBody(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		configured, readOnly bool
		filename             string
		want                 int
		code                 string
	}{
		{"unconfigured", false, false, "shot.png", 503, "docbank_not_configured"},
		{"read-only", true, true, "shot.png", 409, "federated_read_only"},
		{"invalid name", true, false, "bad%FF.png", 400, "attachment_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, f := attachmentEnv(t)
			p, iss := seedProjectAndIssue(t, env)
			if tc.readOnly {
				_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{
					ProjectID: p.ID, Role: db.FederationRoleSpoke, Enabled: true, PushEnabled: false, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: p.UID,
				})
				require.NoError(t, err)
			}
			cfg := daemon.ServerConfig{DB: env.DB}
			if tc.configured {
				cfg.Docbank = docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
				cfg.DocbankFolder = "/kata"
			}
			body := &attachmentUnreadBody{}
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments?name=%s&actor=operator", p.ID, iss.ShortID, tc.filename)
			req := httptest.NewRequest("POST", endpoint, body)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Content-Type", "application/octet-stream")
			res := httptest.NewRecorder()
			daemon.NewServer(cfg).Handler().ServeHTTP(res, req)
			require.Equal(t, tc.want, res.Code, res.Body.String())
			require.Contains(t, res.Body.String(), tc.code)
			require.Zero(t, body.reads)
			require.Empty(t, f.Requests())
		})
	}
}

type attachmentZeros struct{}

func (attachmentZeros) Read(b []byte) (int, error) { clear(b); return len(b), nil }

// TestAttachmentOversizeBeforeVault streams 256 MiB through a temporary file.
func TestAttachmentOversizeBeforeVault(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
	body := io.LimitReader(attachmentZeros{}, (256<<20)+1)
	status, raw := attachmentUpload(t, env, endpoint, "shot.png", body)
	require.Equal(t, 413, status, string(raw))
	require.Contains(t, string(raw), "attachment_too_large")
	stored, err := env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(stored.Metadata))
	require.Empty(t, f.Requests())
}
func TestAttachmentContentTypesAndReportedLength(t *testing.T) {
	for _, tc := range []struct{ name, typ string }{
		{"SHOT.PNG", "image/png"}, {"clip.mov", "video/quicktime"},
		{"shot.svg", "application/octet-stream"}, {"file.unknown", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, f := attachmentEnv(t)
			p, iss := seedProjectAndIssue(t, env)
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
			status, raw := attachmentUpload(t, env, endpoint, tc.name, strings.NewReader("bytes"))
			require.Equal(t, 200, status, string(raw))
			var out struct {
				ID    string              `json:"attachment_id"`
				Entry metadata.Attachment `json:"attachment"`
			}
			require.NoError(t, json.Unmarshal(raw, &out))
			out.Entry.Size = 999 // Editable metadata must not truncate or extend the HTTP response.
			raw, err := json.Marshal(out.Entry)
			require.NoError(t, err)
			_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment." + out.ID: raw}})
			require.NoError(t, err)
			calls := len(f.Requests())
			resp, err := env.HTTP.Get(env.URL + endpoint + "/" + out.ID + "/content")
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "bytes", string(data))
			require.EqualValues(t, 5, resp.ContentLength)
			require.Equal(t, tc.typ, resp.Header.Get("Content-Type"))
			require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
			require.Empty(t, resp.Header.Get("Set-Cookie"))
			require.Len(t, f.Requests(), calls+1, "a read only fetches the immutable version")
			if tc.typ == "application/octet-stream" {
				require.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")
			}
		})
	}
}
func TestAttachmentMalformedReferenceAndReadErrors(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	response, err := env.HTTP.Post(env.URL+fmt.Sprintf("/api/v1/projects/%d/issues/%s/metadata", p.ID, iss.ShortID), "application/json", strings.NewReader(`{"actor":"operator","patch":{"attachment.x":"junk"}}`))
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	code := response.StatusCode
	require.Equal(t, 200, code, string(raw))
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID)
	code, raw = env.Get(t, endpoint+"/x/content")
	require.Equal(t, 400, code, string(raw))
	require.Contains(t, string(raw), "attachment_invalid")
	require.Empty(t, f.Requests())
	code, raw = env.Get(t, endpoint+"/missing/content")
	require.Equal(t, 404, code, string(raw))
	code, raw = attachmentUpload(t, env, endpoint, "shot.png", strings.NewReader("image"))
	require.Equal(t, 200, code, string(raw))
	var out struct {
		ID string `json:"attachment_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	for _, tc := range []struct {
		status, want int
		code         string
	}{{404, 410, "attachment_content_missing"}, {401, 502, "docbank_unauthorized"}, {403, 502, "docbank_unauthorized"}, {503, 502, "docbank_unreachable"}} {
		f.Fault = fmt.Sprintf("status:%d", tc.status)
		code, raw = env.Get(t, endpoint+"/"+out.ID+"/content")
		require.Equal(t, tc.want, code, string(raw))
		require.Contains(t, string(raw), tc.code)
	}
}
func TestAttachmentScopeBeforeOutbound(t *testing.T) {
	f := docbanktest.New(t)
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity(), func(cfg *daemon.ServerConfig) {
		cfg.Docbank = docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
		cfg.DocbankFolder = "/kata"
	})
	p, root := seedProjectAndIssue(t, env)
	hidden := createScopedHTTPTestIssue(t, env, p.ID, "Hidden", nil)
	expires := time.Now().Add(time.Hour)
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "worker-token", Actor: "operator", AdminActor: db.BootstrapActor, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: p.UID, RootIssueUID: root.UID}, ExpiresAt: &expires})
	require.NoError(t, err)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, hidden.ShortID)
	code, raw := attachmentUpload(t, env, endpoint, "shot.png", strings.NewReader("x"), "worker-token")
	require.Equal(t, 404, code, string(raw))
	require.Empty(t, f.Requests())
}
func TestUIAttachmentCapabilityFollowsDocbankConfiguration(t *testing.T) {
	for _, configured := range []bool{false, true} {
		var env *testenv.Env
		if configured {
			env, _ = attachmentEnv(t)
		} else {
			env = testenv.New(t)
		}
		code, raw := env.Get(t, "/api/v1/ui/snapshot?view=inbox")
		require.Equal(t, 200, code, string(raw))
		var snapshot struct {
			Capabilities api.UICapabilities `json:"capabilities"`
		}
		require.NoError(t, json.Unmarshal(raw, &snapshot))
		require.Equal(t, configured, snapshot.Capabilities.Attachments)
	}
}

func TestAttachmentUploadStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status, want  int
		code          string
		folderFailure bool
	}{
		{"413", 413, 413, "attachment_too_large", false},
		{"400", 400, 400, "attachment_invalid", false},
		{"404", 404, 400, "attachment_invalid", false},
		{"500", 500, 502, "docbank_unreachable", false},
		{"folder step fails", 503, 502, "docbank_unreachable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, f := attachmentEnv(t)
			p, iss := seedProjectAndIssue(t, env)
			if tc.folderFailure {
				f.Fault = "status:503"
			} else {
				f.UploadStatus = tc.status
			}
			code, raw := attachmentUpload(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", p.ID, iss.ShortID), "shot.png", strings.NewReader("image"))
			if tc.folderFailure {
				require.NotContains(t, f.Requests(), "POST /api/v1/uploads")
			} else {
				require.Contains(t, f.Requests(), "POST /api/v1/uploads")
			}
			require.Equal(t, tc.want, code, string(raw))
			require.Contains(t, string(raw), tc.code)
			stored, err := env.DB.IssueByID(t.Context(), iss.ID)
			require.NoError(t, err)
			require.JSONEq(t, `{}`, string(stored.Metadata))
		})
	}
}
