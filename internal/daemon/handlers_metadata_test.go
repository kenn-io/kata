package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func seedProjectAndIssue(t *testing.T, env *testenv.Env) (db.Project, db.Issue) {
	t.Helper()
	p := seedProject(t, env, "mp")
	iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
	})
	require.NoError(t, err)
	return p, iss
}

// metadataSubject parameterises the issue + project metadata tests over a
// common setup that returns the patch URL and starting revision for the
// subject entity. The PATCH wire shape is identical across both subjects;
// envKey is the top-level response JSON key ("issue" or "project") under
// which the entity's metadata and revision are nested so the test body
// can decode both response shapes without hand-rolling per-subject structs.
type metadataSubject struct {
	name   string
	envKey string
	setup  func(t *testing.T, env *testenv.Env) (url string, rev int64)
}

func metadataSubjects() []metadataSubject {
	return []metadataSubject{
		{
			"issue", "issue",
			func(t *testing.T, env *testenv.Env) (string, int64) {
				p, iss := seedProjectAndIssue(t, env)
				return fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata",
					env.URL, p.ID, iss.ShortID), iss.Revision
			},
		},
		{
			"project", "project",
			func(t *testing.T, env *testenv.Env) (string, int64) {
				p := seedProject(t, env, "proj")
				return fmt.Sprintf("%s/api/v1/projects/%d/metadata",
					env.URL, p.ID), p.Revision
			},
		},
	}
}

// decodeMetadataEnvelope pulls the {metadata, revision} pair from the
// subject-specific envelope key in a PATCH response body. metadata is a JSON
// object on the wire; the helper returns it as the raw bytes (rendered as a
// string for substring assertions on the well-formed marshal output).
func decodeMetadataEnvelope(t *testing.T, raw []byte, envKey string) (metadata string, rev int64) {
	t.Helper()
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &envelope))
	var inner struct {
		Metadata json.RawMessage `json:"metadata"`
		Revision int64           `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(envelope[envKey], &inner))
	return string(inner.Metadata), inner.Revision
}

// TestPatchMetadata_HappyPath_200 covers the happy-path PATCH for both the
// issue and project metadata endpoints. Each subject supplies its own URL,
// starting revision, a registered key/value to patch, and the substring to
// assert in the persisted metadata blob.
func TestPatchMetadata_HappyPath_200(t *testing.T) {
	cases := []struct {
		subject metadataSubject
		patch   string // value inside `"patch": {...}`
		expect  string // substring required in the decoded metadata blob
	}{
		{metadataSubjects()[0], `{"scheduled_on":"2026-05-20"}`, `"scheduled_on":"2026-05-20"`},
		{metadataSubjects()[1], `{"area":"Personal"}`, `"area":"Personal"`},
	}
	for _, c := range cases {
		t.Run(c.subject.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			url, rev := c.subject.setup(t, env)
			ifMatch := fmt.Sprintf(`"rev-%d"`, rev)
			body := fmt.Sprintf(`{"actor":"tester","patch":%s}`, c.patch)
			resp := doPostWithIfMatch(t, env, url, body, ifMatch)
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)
			require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
			assert.Equal(t, fmt.Sprintf(`"rev-%d"`, rev+1), resp.Header.Get("ETag"))

			metadata, newRev := decodeMetadataEnvelope(t, raw, c.subject.envKey)
			assert.Equal(t, rev+1, newRev)
			assert.Contains(t, metadata, c.expect)
			assert.Contains(t, string(raw), `"changed":true`)
		})
	}
}

// TestPatchMetadata_PresentEmptyIfMatch_400 pins that a present-but-empty
// If-Match header is treated as a malformed conditional write, not as an
// absent header. Huma's typed binding reads headers via Header.Get, which
// collapses absent and present-empty to "", so an empty conditional would
// otherwise be silently downgraded to an unconditional last-write-wins patch.
// It must be rejected with the standard 400 validation error instead.
func TestPatchMetadata_PresentEmptyIfMatch_400(t *testing.T) {
	for _, subject := range metadataSubjects() {
		t.Run(subject.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			url, _ := subject.setup(t, env)

			req, err := http.NewRequest(http.MethodPost, url,
				strings.NewReader(`{"actor":"tester","patch":{}}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer tok")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", "") // present but empty
			resp, err := env.HTTP.Do(req)  //nolint:gosec // G704: test server URL.
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			body, _ := io.ReadAll(resp.Body)
			require.Equalf(t, http.StatusBadRequest, resp.StatusCode, "body: %s", body)
		})
	}
}

// TestPatchMetadata_NullTopLevelPatchRejected pins the wire contract for the
// patch field on both subjects: it must be a JSON object, never null. The
// Huma schema validator rejects `"patch":null` upfront with 400 — this test
// locks that behavior so a future tri-state refactor cannot accidentally
// loosen the schema and let null reach the handler (where a nil map would
// silently no-op). Per-key delete is `{"k":null}` inside the object; null at
// the top level is always invalid.
func TestPatchMetadata_NullTopLevelPatchRejected(t *testing.T) {
	for _, subject := range metadataSubjects() {
		t.Run(subject.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			url, rev := subject.setup(t, env)
			ifMatch := fmt.Sprintf(`"rev-%d"`, rev)
			resp := doPostWithIfMatch(t, env, url, `{"actor":"tester","patch":null}`, ifMatch)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestPatchMetadata_StaleIfMatch_412 covers stale If-Match for both subjects.
func TestPatchMetadata_StaleIfMatch_412(t *testing.T) {
	cases := []struct {
		subject metadataSubject
		patch   string
	}{
		{metadataSubjects()[0], `{"scheduled_on":"2026-05-20"}`},
		{metadataSubjects()[1], `{"area":"Work"}`},
	}
	for _, c := range cases {
		t.Run(c.subject.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			url, _ := c.subject.setup(t, env)
			body := fmt.Sprintf(`{"actor":"tester","patch":%s}`, c.patch)
			resp := doPostWithIfMatch(t, env, url, body, `"rev-99"`)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
		})
	}
}

// TestPatchMetadata_InvalidValueOnKnownKey_400 covers the validator path for
// both subjects: a registered key with a wrong JSON type must produce a 400
// envelope, not a 500.
func TestPatchMetadata_InvalidValueOnKnownKey_400(t *testing.T) {
	cases := []struct {
		subject metadataSubject
		patch   string // registered key with a value of the wrong JSON type
	}{
		{metadataSubjects()[0], `{"scheduled_on":123}`}, // scheduled_on expects a date string
		{metadataSubjects()[1], `{"area":123}`},         // area is TypeString
	}
	for _, c := range cases {
		t.Run(c.subject.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			url, rev := c.subject.setup(t, env)
			ifMatch := fmt.Sprintf(`"rev-%d"`, rev)
			body := fmt.Sprintf(`{"actor":"tester","patch":%s}`, c.patch)
			resp := doPostWithIfMatch(t, env, url, body, ifMatch)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestPatchIssueMetadata_UnknownKey_Accepted: the daemon doesn't enforce a
// closed metadata schema. Unknown keys are written through as opaque values
// so consumers can carry their own UI hints without daemon releases.
func TestPatchIssueMetadata_UnknownKey_Accepted(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p, iss := seedProjectAndIssue(t, env)

	url := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata", env.URL, p.ID, iss.ShortID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)
	resp := doPostWithIfMatch(t, env, url, `{"actor":"tester","patch":{"definitely_not_a_key":"yellow"}}`, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	assert.Equal(t, fmt.Sprintf(`"rev-%d"`, iss.Revision+1), resp.Header.Get("ETag"))

	var out struct {
		Issue struct {
			Metadata json.RawMessage `json:"metadata"`
			Revision int64           `json:"revision"`
		} `json:"issue"`
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.True(t, out.Changed)
	assert.Equal(t, iss.Revision+1, out.Issue.Revision)
	assert.Contains(t, string(out.Issue.Metadata), `"definitely_not_a_key":"yellow"`,
		"unknown key must round-trip into the persisted metadata blob")

	// GET-after view confirms the key is durably stored and surfaced.
	getReq, err := http.NewRequest(http.MethodGet, url[:len(url)-len("/metadata")], nil)
	require.NoError(t, err)
	getReq.Header.Set("Authorization", "Bearer tok")
	getResp, err := env.HTTP.Do(getReq) //nolint:gosec // G704: test server URL, not user-controlled
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	getBody, _ := io.ReadAll(getResp.Body)
	require.Equalf(t, http.StatusOK, getResp.StatusCode, "GET body: %s", getBody)

	var view struct {
		Issue struct {
			Metadata json.RawMessage `json:"metadata"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(getBody, &view))
	assert.Contains(t, string(view.Issue.Metadata), `"definitely_not_a_key":"yellow"`,
		"GET-after must surface the opaque key alongside the reserved ones")
}

// TestPatchMetadata_NoIfMatch_UnconditionalWrite pins the concurrency contract
// for both subjects: If-Match is OPTIONAL on the metadata patch endpoints.
// Without it the patch is unconditional last-write-wins — the intended default
// for convention keys (e.g. work.attention) whose writers must never see a
// spurious 412 from a concurrent same-key update. The write must succeed even
// when the entity's revision has advanced past its creation value; a caller
// that genuinely needs read-modify-write opts in by sending If-Match.
func TestPatchMetadata_NoIfMatch_UnconditionalWrite(t *testing.T) {
	for _, subject := range metadataSubjects() {
		t.Run(subject.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			url, rev := subject.setup(t, env)

			// Advance the revision with a conditional patch so the follow-up
			// unconditional write cannot accidentally match the initial rev.
			resp := doPostWithIfMatch(t, env, url,
				`{"actor":"tester","patch":{"advance_marker":"one"}}`,
				fmt.Sprintf(`"rev-%d"`, rev))
			raw := readClose(t, resp)
			require.Equalf(t, http.StatusOK, resp.StatusCode, "setup patch body: %s", raw)

			resp = doPostWithIfMatch(t, env, url,
				`{"actor":"tester","patch":{"work.attention":"needs-human"}}`, "")
			raw = readClose(t, resp)
			require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
			assert.Equal(t, fmt.Sprintf(`"rev-%d"`, rev+2), resp.Header.Get("ETag"))

			metadata, newRev := decodeMetadataEnvelope(t, raw, subject.envKey)
			assert.Equal(t, rev+2, newRev)
			assert.Contains(t, metadata, `"work.attention":"needs-human"`)
			assert.Contains(t, string(raw), `"changed":true`)
		})
	}
}

// TestPatchProjectMetadata_UnknownKey_Accepted mirrors the issue test: the
// project metadata blob also accepts unknown keys opaquely. The ShowProject
// wire shape does not project the metadata blob, so durable persistence is
// verified by re-reading the project row directly.
func TestPatchProjectMetadata_UnknownKey_Accepted(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p, err := env.DB.CreateProject(t.Context(), "proj3")
	require.NoError(t, err)

	url := fmt.Sprintf("%s/api/v1/projects/%d/metadata", env.URL, p.ID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, p.Revision)
	resp := doPostWithIfMatch(t, env, url, `{"actor":"tester","patch":{"definitely_not_a_key":"yellow"}}`, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	assert.Equal(t, fmt.Sprintf(`"rev-%d"`, p.Revision+1), resp.Header.Get("ETag"))

	var out struct {
		Project struct {
			Metadata json.RawMessage `json:"metadata"`
			Revision int64           `json:"revision"`
		} `json:"project"`
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.True(t, out.Changed)
	assert.Equal(t, p.Revision+1, out.Project.Revision)
	assert.Contains(t, string(out.Project.Metadata), `"definitely_not_a_key":"yellow"`,
		"unknown key must round-trip into the persisted metadata blob")

	// DB-side check: confirm the key is durably stored (the ShowProject wire
	// shape doesn't expose the metadata blob, so we re-read the row).
	stored, err := env.DB.ProjectByID(t.Context(), p.ID)
	require.NoError(t, err)
	assert.Contains(t, string(stored.Metadata), `"definitely_not_a_key":"yellow"`,
		"opaque key must survive a fresh DB read")
}

// TestPatchIssueMetadata_Broadcasts pins that a metadata patch that actually
// changes something wakes SSE followers: the handler must broadcast the
// issue.metadata_updated event through cfg.Broadcaster (the same path
// kata events --tail subscribes to). A no-op patch must NOT broadcast.
func TestPatchIssueMetadata_Broadcasts(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p, iss := seedProjectAndIssue(t, env)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()

	url := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata", env.URL, p.ID, iss.ShortID)
	resp := doPostWithIfMatch(t, env, url,
		`{"actor":"tester","patch":{"scheduled_on":"2026-05-20"}}`,
		fmt.Sprintf(`"rev-%d"`, iss.Revision))
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", readClose(t, resp))

	msg := receiveMsg(t, sub.Ch, time.Second, "issue metadata patch broadcast")
	require.NotNil(t, msg.Event)
	assert.Equal(t, "issue.metadata_updated", msg.Event.Type)
	assert.Equal(t, p.ID, msg.ProjectID)

	// A no-op patch (deleting an absent key) must not broadcast.
	resp = doPostWithIfMatch(t, env, url,
		`{"actor":"tester","patch":{"nonexistent_key":null}}`, "")
	raw := readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "no-op body: %s", raw)
	assert.Contains(t, string(raw), `"changed":false`, "no-op patch must report changed=false")
	assertNoReceive(t, sub.Ch, 200*time.Millisecond, "no-op issue metadata patch must not broadcast")
}

// TestPatchProjectMetadata_Broadcasts is the project-subject mirror: a changing
// project metadata patch must broadcast project.metadata_updated, and a no-op
// patch must stay silent.
func TestPatchProjectMetadata_Broadcasts(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "bcast")
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()

	url := fmt.Sprintf("%s/api/v1/projects/%d/metadata", env.URL, p.ID)
	resp := doPostWithIfMatch(t, env, url,
		`{"actor":"tester","patch":{"area":"Work"}}`,
		fmt.Sprintf(`"rev-%d"`, p.Revision))
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", readClose(t, resp))

	msg := receiveMsg(t, sub.Ch, time.Second, "project metadata patch broadcast")
	require.NotNil(t, msg.Event)
	assert.Equal(t, "project.metadata_updated", msg.Event.Type)
	assert.Equal(t, p.ID, msg.ProjectID)

	resp = doPostWithIfMatch(t, env, url,
		`{"actor":"tester","patch":{"nonexistent_key":null}}`, "")
	raw := readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "no-op body: %s", raw)
	assert.Contains(t, string(raw), `"changed":false`, "no-op patch must report changed=false")
	assertNoReceive(t, sub.Ch, 200*time.Millisecond, "no-op project metadata patch must not broadcast")
}

func TestPatchProjectMetadata_DesignatesInboxAtomically(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	previous := seedProject(t, env, "previous-inbox")
	target := seedProject(t, env, "example-project")
	_, err := env.DB.PatchProjectMetadata(t.Context(), db.PatchProjectMetadataIn{
		ProjectID: previous.ID,
		Actor:     "user-a",
		Patch:     map[string]json.RawMessage{"role": json.RawMessage(`"inbox"`)},
	})
	require.NoError(t, err)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()

	url := fmt.Sprintf("%s/api/v1/projects/%d/metadata", env.URL, target.ID)
	resp := doPostWithIfMatch(t, env, url,
		`{"actor":"user-a","patch":{"role":"inbox"}}`,
		fmt.Sprintf(`"rev-%d"`, target.Revision))
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", readClose(t, resp))

	projectIDs := map[int64]bool{}
	for range 2 {
		msg := receiveMsg(t, sub.Ch, time.Second, "Inbox designation broadcast")
		require.NotNil(t, msg.Event)
		projectIDs[msg.ProjectID] = true
	}
	assert.Equal(t, map[int64]bool{previous.ID: true, target.ID: true}, projectIDs)
	previous, err = env.DB.ProjectByID(t.Context(), previous.ID)
	require.NoError(t, err)
	target, err = env.DB.ProjectByID(t.Context(), target.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(previous.Metadata))
	assert.JSONEq(t, `{"role":"inbox"}`, string(target.Metadata))
}
