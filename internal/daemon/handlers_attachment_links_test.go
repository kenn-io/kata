package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/testenv"
)

const linkVersion = "6f1c2e8a-2222-4222-8222-222222222222"
const oldLinkVersion = "6f1c2e8a-2222-4222-8222-222222222223"

func seedLinkFile(f *docbanktest.Fake) {
	f.Seed("/reports/report.pdf", docbank.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: uuid.MustParse(linkVersion)}, map[uuid.UUID][]byte{uuid.MustParse(linkVersion): []byte("sample bytes"), uuid.MustParse(oldLinkVersion): []byte("old pdf")})
}

func TestAttachmentLinkPinsExistingVersion(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	seedLinkFile(f)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID)
	code, raw, err := postAttachmentLink(t, env, endpoint, `{"path":"/reports/report.pdf","actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	var out api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, "report.pdf", out.Attachment.Name)
	require.Equal(t, linkVersion, out.Attachment.DocbankVersion.String())
	require.EqualValues(t, 12, out.Attachment.Size)
	require.Equal(t, []string{"GET /api/v1/path"}, f.Requests())
	code, raw, err = postAttachmentLink(t, env, endpoint, `{"node_id":42,"actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, linkVersion, out.Attachment.DocbankVersion.String())
	require.EqualValues(t, 12, out.Attachment.Size)
	require.Equal(t, []string{"GET /api/v1/path", "GET /api/v1/nodes/42"}, f.Requests())
	code, raw, err = postAttachmentLink(t, env, endpoint, `{"node_id":42,"version_id":"`+oldLinkVersion+`","actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, oldLinkVersion, out.Attachment.DocbankVersion.String())
	require.EqualValues(t, 7, out.Attachment.Size)
	contentRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/attachments/%s/content", env.URL, p.ID, iss.ShortID, out.ID), nil)
	require.NoError(t, err)
	resp, err := env.HTTP.Do(contentRequest)
	require.NoError(t, err)
	content, err := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "old pdf", string(content))
	for _, request := range f.Requests() {
		require.True(t, strings.HasPrefix(request, "GET "), request)
	}
}

func TestAttachmentWritesRefreshStaleFederatedClaimDenials(t *testing.T) {
	for _, operation := range []string{"upload", "link"} {
		t.Run(operation, func(t *testing.T) {
			f := docbanktest.New(t)
			if operation == "link" {
				f.Seed("/kata/report.pdf", docbank.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: uuid.MustParse(linkVersion)}, map[uuid.UUID][]byte{
					uuid.MustParse(linkVersion): []byte("sample bytes"),
				})
			}
			hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim,push,pull", func(cfg *daemon.ServerConfig) {
				cfg.Docbank = docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
				cfg.DocbankFolder = "/kata"
			})
			require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
				HubURL: hub.URL, HubProjectID: hubProject.ID, Token: token, Capabilities: "claim,push,pull",
			}))

			ctx := t.Context()
			holder := db.ClaimPrincipal{HolderInstanceUID: claimTestOtherSpokeUID, Holder: "other-spoke", ClientKind: "cli"}
			_, err := hub.DB.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: hubProject.ID, IssueRef: issue.UID, Principal: holder, ClaimKind: "hard", Now: time.Now().UTC(),
			})
			require.NoError(t, err)
			stale, err := hub.DB.ClaimStatusReadOnly(ctx, hubProject.ID, issue.UID, time.Now().UTC())
			require.NoError(t, err)
			require.True(t, stale.Held)
			require.NoError(t, spoke.DB.ApplyClaimStatus(ctx, spokeProject.ID, issue.UID, stale))
			_, err = hub.DB.ReleaseClaim(ctx, db.ReleaseClaimParams{
				ProjectID: hubProject.ID, IssueRef: issue.UID, Principal: holder, Reason: "done", Now: time.Now().UTC(),
			})
			require.NoError(t, err)

			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", spokeProject.ID, issue.ShortID)
			var status int
			var raw []byte
			if operation == "upload" {
				status, raw = attachmentUpload(t, spoke, endpoint, "shot.png", strings.NewReader("image bytes"))
			} else {
				var err error
				status, raw, err = postAttachmentLink(t, spoke, endpoint+"/links", `{"path":"/kata/report.pdf","actor":"operator","client_kind":"cli"}`)
				require.NoError(t, err)
			}
			require.Equal(t, http.StatusOK, status, string(raw))
			var out api.AddAttachmentResponseBody
			require.NoError(t, json.Unmarshal(raw, &out))
			require.Equal(t, hub.DB.InstanceUID(), out.Attachment.Origin)
		})
	}
}

func TestFederatedAttachmentUploadPreservesBodyLimitError(t *testing.T) {
	f := docbanktest.New(t)
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim,push,pull", func(cfg *daemon.ServerConfig) {
		cfg.Docbank = docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
		cfg.DocbankFolder = "/kata"
	})
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL: hub.URL, HubProjectID: hubProject.ID, Token: token, Capabilities: "claim,push,pull",
	}))

	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", spokeProject.ID, issue.ShortID)
	status, raw := attachmentUpload(t, spoke, endpoint, "shot.png", io.LimitReader(attachmentZeros{}, (256<<20)+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, status, string(raw))
	require.Contains(t, string(raw), "attachment_too_large")
	require.Empty(t, f.Requests(), "oversized upload must be rejected before reaching Docbank")
}

func TestAttachmentLinkCanReuseAnExistingVersion(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	seedLinkFile(f)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID)
	code, raw, err := postAttachmentLink(t, env, endpoint, `{"path":"/reports/report.pdf","actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	var first api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &first))
	afterFirst, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	code, raw, err = postAttachmentLink(t, env, endpoint, `{"node_id":42,"reuse_existing":true,"actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	var second api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &second))
	require.Equal(t, first.ID, second.ID)
	afterSecond, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, afterFirst, afterSecond, "relinking the same version must not add a metadata event")

	code, raw, err = postAttachmentLink(t, env, endpoint, `{"node_id":42,"actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	var third api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &third))
	require.NotEqual(t, first.ID, third.ID, "standalone linking continues to create a new reference")
	afterThird, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Greater(t, afterThird, afterSecond)
}

func TestAttachmentLinkReuseChoosesSmallestExistingID(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	seedLinkFile(f)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID)

	code, raw, err := postAttachmentLink(t, env, endpoint, `{"node_id":42,"actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	var first api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &first))

	code, raw, err = postAttachmentLink(t, env, endpoint, `{"node_id":42,"actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	var second api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &second))
	wantID := min(first.ID, second.ID)

	code, raw, err = postAttachmentLink(t, env, endpoint, `{"node_id":42,"reuse_existing":true,"actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, 200, code, string(raw))
	var reused api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &reused))
	require.Equal(t, wantID, reused.ID)
}

func TestAttachmentLinkReuseHandlesConcurrentRequests(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	seedLinkFile(f)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	bothResolved := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var requests atomic.Int32
	f.BeforeResponse = func(method, path string) {
		if method != http.MethodGet || path != "/api/v1/nodes/42" {
			return
		}
		if requests.Add(1) == 2 {
			close(bothResolved)
		}
		<-release
	}
	defer releaseOnce.Do(func() { close(release) })

	type response struct {
		status int
		body   []byte
		err    error
	}
	responses := make(chan response, 2)
	for range 2 {
		go func() {
			status, body, err := postAttachmentLink(t, env, endpoint, `{"node_id":42,"reuse_existing":true,"actor":"operator"}`)
			responses <- response{status: status, body: body, err: err}
		}()
	}
	select {
	case <-bothResolved:
		releaseOnce.Do(func() { close(release) })
	case <-time.After(5 * time.Second):
		t.Fatal("both requests did not reach the Docbank lookup")
	}

	var ids []string
	for range 2 {
		got := <-responses
		require.NoError(t, got.err)
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		var result api.AddAttachmentResponseBody
		require.NoError(t, json.Unmarshal(got.body, &result))
		ids = append(ids, result.ID)
	}
	require.Equal(t, ids[0], ids[1], "concurrent retries must reuse one attachment reference")
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before+1, after, "concurrent reuse must emit one metadata event total")
}

func TestAttachmentLinkReuseRefreshesReferenceAfterConcurrentRemoval(t *testing.T) {
	env, f := attachmentEnv(t)
	p, iss := seedProjectAndIssue(t, env)
	seedLinkFile(f)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID)

	code, raw, err := postAttachmentLink(t, env, endpoint, `{"node_id":42,"actor":"operator"}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code, string(raw))
	var first api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(raw, &first))

	lookupStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	f.BeforeResponse = func(method, path string) {
		if method == http.MethodGet && path == "/api/v1/nodes/42" {
			close(lookupStarted)
			<-release
		}
	}
	defer releaseOnce.Do(func() { close(release) })

	type response struct {
		status int
		body   []byte
		err    error
	}
	responses := make(chan response, 1)
	go func() {
		status, body, err := postAttachmentLink(t, env, endpoint, `{"node_id":42,"reuse_existing":true,"actor":"operator"}`)
		responses <- response{status: status, body: body, err: err}
	}()
	select {
	case <-lookupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("reuse request did not reach the Docbank lookup")
	}

	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: iss.ID, Actor: "operator",
		Patch: map[string]jsontext.Value{"attachment." + first.ID: jsontext.Value(`null`)},
	})
	require.NoError(t, err)
	releaseOnce.Do(func() { close(release) })

	got := <-responses
	require.NoError(t, got.err)
	require.Equal(t, http.StatusOK, got.status, string(got.body))
	var reused api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(got.body, &reused))
	require.NotEqual(t, first.ID, reused.ID, "reuse must observe a concurrent reference removal")

	current, err := env.DB.IssueByUID(t.Context(), iss.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	var attachments map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(current.Metadata), &attachments))
	require.NotContains(t, attachments, "attachment."+first.ID)
	require.Contains(t, attachments, "attachment."+reused.ID)
}

func postAttachmentLink(t *testing.T, env *testenv.Env, endpoint, body string) (int, []byte, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+endpoint, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(req)
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
		expectedRequests                         []string
	}{
		{name: "empty", body: `{}`, status: 400, code: "attachment_invalid"},
		{name: "both", body: `{"node_id":42,"path":"/reports/report.pdf"}`, status: 400, code: "attachment_invalid"},
		{name: "zero", body: `{"node_id":0}`, status: 400, code: "attachment_invalid"},
		{name: "upstream rejects path", body: `{"path":"report.pdf"}`, fault: "status:400", status: 400, code: "attachment_invalid", expectedRequests: []string{"GET /api/v1/path"}},
		{name: "directory", kind: "dir", status: 400, code: "attachment_not_file"},
		{name: "trashed", trash: "2026-10-25T01:00:00Z", status: 410, code: "attachment_content_missing"},
		{name: "wrong node", body: `{"node_id":42,"version_id":"` + oldLinkVersion + `"}`, nodeID: 43, status: 400, code: "attachment_version_mismatch"},
		{name: "bad name", fileName: strings.Repeat("a", 256), status: 400, code: "attachment_invalid"},
		{name: "missing node", body: `{"node_id":99}`, status: 404, code: "docbank_not_found"},
		{name: "missing path", body: `{"path":"/reports/missing.pdf"}`, status: 404, code: "docbank_not_found"},
		{name: "missing version", body: `{"node_id":42,"version_id":"` + oldLinkVersion + `"}`, status: 404, code: "docbank_not_found"},
		{name: "unauthorized", fault: "status:401", status: 502, code: "docbank_unauthorized"},
		{name: "forbidden", fault: "status:403", status: 502, code: "docbank_unauthorized"},
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
			node := docbank.Node{ID: 42, Name: tc.fileName, Kind: tc.kind, TrashedAt: tc.trash}
			if tc.kind == "file" {
				node.CurrentVersionID = uuid.MustParse(linkVersion)
				f.Seed("/reports/report.pdf", node, map[uuid.UUID][]byte{uuid.MustParse(linkVersion): []byte("sample bytes")})
			} else {
				f.Seed("/reports/report.pdf", node, nil)
			}
			if tc.nodeID != 42 {
				f.Seed("/reports/other.pdf", docbank.Node{ID: tc.nodeID, Name: "other.pdf", Kind: "file", CurrentVersionID: uuid.MustParse(oldLinkVersion)}, map[uuid.UUID][]byte{uuid.MustParse(oldLinkVersion): []byte("old pdf")})
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
			status, raw, err := postAttachmentLink(t, env, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/links", p.ID, iss.ShortID), string(rawInput))
			require.NoError(t, err)
			assertAPIError(t, status, raw, tc.status, tc.code)
			stored, err := env.DB.IssueByID(t.Context(), iss.ID)
			require.NoError(t, err)
			require.JSONEq(t, `{}`, string(stored.Metadata))
			if tc.expectedRequests != nil {
				require.Equal(t, tc.expectedRequests, f.Requests())
			} else if tc.status == 400 && tc.body != "" && tc.nodeID == 42 {
				require.Empty(t, f.Requests(), "invalid selector must fail before upstream reads")
			}
		})
	}
}
