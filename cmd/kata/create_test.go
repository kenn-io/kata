package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
)

func TestCreateRequestError(t *testing.T) {
	timeoutErr := &url.Error{
		Op:  http.MethodPost,
		URL: "https://daemon.example/api/v1/projects/7/issues",
		Err: context.DeadlineExceeded,
	}

	got := createRequestError(timeoutErr, false)
	var cliErr *cliError
	require.ErrorAs(t, got, &cliErr)
	assert.Equal(t, kindInternal, cliErr.Kind)
	assert.Equal(t, "create_outcome_unknown", cliErr.Code)
	assert.Equal(t, ExitInternal, cliErr.ExitCode)
	assert.Contains(t, cliErr.Message, "timed out")
	assert.Contains(t, cliErr.Message, "check whether the issue was created")
	assert.Contains(t, cliErr.Message, "--force-new")
	assert.NotContains(t, cliErr.Message, "daemon.example")

	forceGot := createRequestError(timeoutErr, true)
	var forceErr *cliError
	require.ErrorAs(t, forceGot, &forceErr)
	assert.Equal(t, "create_outcome_unknown", forceErr.Code)
	assert.Contains(t, forceErr.Message, "check whether the issue was created")
	assert.NotContains(t, forceErr.Message, "--force-new")
	assert.NotContains(t, forceErr.Message, "daemon.example")

	clientTimeoutErr := &url.Error{
		Op:  http.MethodPost,
		URL: "https://daemon.example/api/v1/projects/7/issues",
		Err: &net.DNSError{IsTimeout: true},
	}
	clientTimeoutGot := createRequestError(clientTimeoutErr, false)
	var clientTimeoutCLIError *cliError
	require.ErrorAs(t, clientTimeoutGot, &clientTimeoutCLIError)
	assert.Equal(t, "create_outcome_unknown", clientTimeoutCLIError.Code)
	assert.Contains(t, clientTimeoutCLIError.Message, "check whether the issue was created")
	assert.NotContains(t, clientTimeoutCLIError.Message, "daemon.example")

	otherErr := errors.New("connection refused")
	assert.Same(t, otherErr, createRequestError(otherErr, false))
}

func TestCreateRequestErrorConnectionDropped(t *testing.T) {
	dropped := &url.Error{Op: http.MethodPost, URL: "https://daemon.example/issues", Err: io.EOF}
	got := createRequestError(dropped, false)
	var cliErr *cliError
	require.ErrorAs(t, got, &cliErr)
	assert.Equal(t, "create_outcome_unknown", cliErr.Code)
	assert.Contains(t, cliErr.Message, "connection dropped")

}

func TestCreateRequestErrorCanceled(t *testing.T) {
	canceledErr := &url.Error{
		Op:  http.MethodPost,
		URL: "https://daemon.example/api/v1/projects/7/issues",
		Err: context.Canceled,
	}

	got := createRequestError(canceledErr, false)
	var cliErr *cliError
	require.ErrorAs(t, got, &cliErr)
	assert.Equal(t, kindInternal, cliErr.Kind)
	assert.Equal(t, "create_outcome_unknown", cliErr.Code)
	assert.Equal(t, ExitInternal, cliErr.ExitCode)
	assert.Contains(t, cliErr.Message, "canceled")
	assert.Contains(t, cliErr.Message, "check whether the issue was created")
	assert.Contains(t, cliErr.Message, "--force-new")
	assert.NotContains(t, cliErr.Message, "timed out")
	assert.NotContains(t, cliErr.Message, "daemon.example")

	forceGot := createRequestError(canceledErr, true)
	var forceErr *cliError
	require.ErrorAs(t, forceGot, &forceErr)
	assert.Equal(t, "create_outcome_unknown", forceErr.Code)
	assert.Contains(t, forceErr.Message, "canceled")
	assert.Contains(t, forceErr.Message, "check whether the issue was created")
	assert.NotContains(t, forceErr.Message, "--force-new")
	assert.NotContains(t, forceErr.Message, "timed out")
	assert.NotContains(t, forceErr.Message, "daemon.example")
}

func TestCreateCanceledClassificationAtCommandBoundary(t *testing.T) {
	createHit := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"example-project"}}`)
		case "/api/v1/projects/7/issues":
			select {
			case createHit <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(contextWithBaseURL(context.Background(), server.URL))
	t.Cleanup(cancel)
	go func() {
		<-createHit
		cancel()
	}()

	_, _, err := executeRootCapture(t, ctx, "--workspace", t.TempDir(),
		"create", "example issue")
	cliErr := requireCLIError(t, err, ExitInternal)
	assert.Equal(t, "create_outcome_unknown", cliErr.Code)
	assert.Contains(t, cliErr.Message, "canceled")
	assert.Contains(t, cliErr.Message, "check whether the issue was created")
	assert.NotContains(t, cliErr.Message, "timed out")
}

func TestCreateTimeoutClassificationAtCommandBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"example-project"}}`)
		case "/api/v1/projects/7/issues":
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	run := func(t *testing.T, extra ...string) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(
			contextWithBaseURL(context.Background(), server.URL), 150*time.Millisecond)
		defer cancel()
		args := []string{"--workspace", t.TempDir(), "create", "example issue"}
		args = append(args, extra...)
		_, _, err := executeRootCapture(t, ctx, args...)
		return err
	}

	t.Run("normal create reports unknown outcome", func(t *testing.T) {
		cliErr := requireCLIError(t, run(t), ExitInternal)
		assert.Equal(t, "create_outcome_unknown", cliErr.Code)
	})

	t.Run("force new reports unknown outcome without retry advice", func(t *testing.T) {
		cliErr := requireCLIError(t, run(t, "--force-new"), ExitInternal)
		assert.Equal(t, "create_outcome_unknown", cliErr.Code)
		assert.NotContains(t, cliErr.Message, "--force-new")
	})
}

func TestCreateResponseBodyCutClassificationAtCommandBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"example-project"}}`)
		case "/api/v1/projects/7/issues":
			_, _ = io.Copy(io.Discard, r.Body)
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n" +
				"Content-Type: application/json\r\n" +
				"Content-Length: 512\r\n\r\n" +
				`{"issue":{"short_id":"abc4","title":"example issue","status":"open"`)
			_ = buf.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	ctx := contextWithBaseURL(context.Background(), server.URL)
	_, _, err := executeRootCapture(t, ctx, "--workspace", t.TempDir(),
		"create", "example issue")
	cliErr := requireCLIError(t, err, ExitInternal)
	assert.Equal(t, "create_outcome_unknown", cliErr.Code)
	assert.Contains(t, cliErr.Message, "cut off")
	assert.Contains(t, cliErr.Message, "check whether the issue was created")
	assert.NotContains(t, cliErr.Message, "timed out")
}

func TestCreate_PrintsIssueShortIDInQuietMode(t *testing.T) {
	env, dir := setupCLIEnv(t)
	out := runCLI(t, env, dir, "--quiet", "create", "first issue", "--body", "details")
	// Quiet mode emits the new issue's short_id as the only output.
	assert.NotEmpty(t, out)
	assert.NotContains(t, out, "\n", "quiet mode must emit a single line")
}

func TestCreate_AgentOutput(t *testing.T) {
	env, dir := setupCLIEnv(t)

	out := runCLI(t, env, dir, "--agent", "create", "first issue")

	assert.Regexp(t, `(?m)^OK create \S+`, out)
	assert.Contains(t, out, `Issue: `)
	assert.Contains(t, out, `Status: open`)
}

func TestCreate_WithInitialLabelsAndParent(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	parent := createIssue(t, env, pid, "parent-issue")
	blocker := createIssue(t, env, pid, "blocker")

	out := runCLI(t, env, dir, "create", "child",
		"--label", "bug", "--label", "needs-review",
		"--parent", parent,
		"--blocks", blocker,
		"--owner", "alice",
	)
	assert.Contains(t, out, "child")

	// Decode the created issue's short_id from the create response so we
	// can fetch and assert on its persisted state.
	type createResp struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	jsonOut := runCLI(t, env, dir, "--json", "create", "child2",
		"--label", "bug",
		"--parent", parent,
		"--blocks", blocker,
		"--owner", "alice",
	)
	var resp createResp
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &resp))

	b := fetchIssueViaHTTP(t, env, pid, resp.Issue.ShortID)
	require.NotNil(t, b.Issue.Owner)
	assert.Equal(t, "alice", *b.Issue.Owner)

	gotLabels := make([]string, 0, len(b.Labels))
	for _, l := range b.Labels {
		gotLabels = append(gotLabels, l.Label)
	}
	assert.Contains(t, gotLabels, "bug")

	var sawParent, sawBlocks bool
	for _, l := range b.Links {
		switch l.Type {
		case "parent":
			if l.From.ShortID == resp.Issue.ShortID && l.To.ShortID == parent {
				sawParent = true
			}
		case "blocks":
			if l.From.ShortID == resp.Issue.ShortID && l.To.ShortID == blocker {
				sawBlocks = true
			}
		}
	}
	assert.True(t, sawParent, "parent link from new issue to parent must be persisted")
	assert.True(t, sawBlocks, "blocks link from new issue to blocker must be persisted")
}

// TestCreate_WithBlockedByAndRelated covers the new repeatable link flags
// added by the relationship-flag consolidation. `--blocked-by R` records
// "this new issue is blocked by R" — i.e. the link runs FROM R TO the new
// issue. `--related R` records the symmetric tie.
func TestCreate_WithBlockedByAndRelated(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	blocker := createIssue(t, env, pid, "blocker")
	peer := createIssue(t, env, pid, "peer")

	type createResp struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	out := runCLI(t, env, dir, "--json", "create", "child",
		"--blocked-by", blocker,
		"--related", peer,
	)
	var resp createResp
	require.NoError(t, json.Unmarshal([]byte(out), &resp))

	b := fetchIssueViaHTTP(t, env, pid, resp.Issue.ShortID)

	var sawBlockedBy, sawRelated bool
	for _, l := range b.Links {
		switch l.Type {
		case "blocks":
			if l.From.ShortID == blocker && l.To.ShortID == resp.Issue.ShortID {
				sawBlockedBy = true
			}
		case "related":
			if (l.From.ShortID == peer && l.To.ShortID == resp.Issue.ShortID) ||
				(l.From.ShortID == resp.Issue.ShortID && l.To.ShortID == peer) {
				sawRelated = true
			}
		}
	}
	assert.True(t, sawBlockedBy, "blocks link from blocker to new issue (blocked-by) must be persisted")
	assert.True(t, sawRelated, "related link between peer and new issue must be persisted")
}

func TestCreate_WithIdempotencyKeyReusesOnRepeat(t *testing.T) {
	env, dir := setupCLIEnv(t)

	// First call.
	first := runCLI(t, env, dir, "--quiet", "create",
		"first issue", "--idempotency-key", "K1")
	assert.NotEmpty(t, first)

	// Repeat with the same key + same fingerprint → reuse, same short_id.
	resetFlags(t)
	second := runCLI(t, env, dir, "--quiet", "create",
		"first issue", "--idempotency-key", "K1")
	assert.Equal(t, first, second, "same key + fingerprint must return existing issue short_id")
}

func TestCreate_AgentOutputIdempotencyReuse(t *testing.T) {
	env, dir := setupCLIEnv(t)

	runCLI(t, env, dir, "--agent", "create", "first issue", "--idempotency-key", "K")
	resetFlags(t)
	second := runCLI(t, env, dir, "--agent", "create", "first issue", "--idempotency-key", "K")

	assert.Contains(t, second, "reused=true changed=false")
}

// TestCreate_IdempotentReuseHumanModeOmitsLinksSummary pins that a
// create whose Idempotency-Key matched a prior issue (changed=false)
// does NOT print a synthetic `links: +parent ...` summary in human
// mode — nothing was mutated on this call, so reporting "links
// applied" would mislead the operator.
func TestCreate_IdempotentReuseHumanModeOmitsLinksSummary(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	parent := createIssue(t, env, pid, "parent")

	// First create with a parent link.
	first := runCLI(t, env, dir, "create",
		"child", "--parent", parent, "--idempotency-key", "K2")
	assert.Contains(t, first, "links: +parent "+parent,
		"sanity: the original create echoes the link summary")

	// Second call with the same key → daemon returns the existing issue
	// with changed=false. The synthesized links summary must NOT print.
	resetFlags(t)
	second := runCLI(t, env, dir, "create",
		"child", "--parent", parent, "--idempotency-key", "K2")
	assert.NotContains(t, second, "links:",
		"idempotent reuse must not synthesize a links summary: %q", second)
}

func TestCreate_ForceNewBypassesLookalike(t *testing.T) {
	env, dir := setupCLIEnv(t)
	first := createIssueViaHTTP(t, env, dir, "fix login crash on Safari")

	// Without --force-new the daemon would 409 on look-alike. With it, a new
	// issue lands with a fresh short_id.
	second := runCLI(t, env, dir, "--quiet", "create",
		"fix login crash Safari", "--force-new")
	assert.NotEmpty(t, second)
	assert.NotEqual(t, first, second)
}

// TestResolveProjectID_PropagatesParseError guards against a malformed
// .kata.toml silently falling through to a start_path request. In
// remote-client mode the daemon cannot stat the client path, so the
// failure mode would be a confusing "stat: no such file" instead of
// the actual "broken .kata.toml" the user can fix. The fix-it error
// must surface client-side without ever calling the daemon.
func TestResolveProjectID_PropagatesParseError(t *testing.T) {
	resetFlags(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), //nolint:gosec // test fixture mode matches production
		[]byte("not = valid = toml ==="), 0o644))

	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called.Add(1)
	}))
	t.Cleanup(srv.Close)

	_, err := resolveProjectID(context.Background(), srv.URL, dir)
	require.Error(t, err)
	assert.Zero(t, called.Load(), "client must reject parse errors before reaching the daemon")
}

// TestResolveProjectID_FallsBackOnMissingConfig confirms the missing
// case still works: when no .kata.toml exists, the request goes
// through with start_path so the daemon can resolve via its own
// filesystem walk (local-mode behavior).
func TestResolveProjectID_FallsBackOnMissingConfig(t *testing.T) {
	resetFlags(t)
	dir := t.TempDir() // no .kata.toml

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bs, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42}}`))
	}))
	t.Cleanup(srv.Close)

	id, err := resolveProjectID(context.Background(), srv.URL, dir)
	require.NoError(t, err)
	assert.EqualValues(t, 42, id)
	assert.Equal(t, dir, got["start_path"])
	_, hasName := got["name"]
	assert.False(t, hasName, "no .kata.toml means no project name in the request")
}

// TestResolveProjectID_SendsNameAndAliasForWorkspaceConfig is the
// regression coverage for issue #35: when .kata.toml is readable, the
// client must derive {name, alias} locally and send a path-free
// request. The daemon's alias-first repair runs against the supplied
// alias, not against a daemon-side filesystem walk that fails on
// remote clients (the bug 12ced3a introduced by collapsing the
// project_identity branch into the always-start_path fallthrough).
func TestResolveProjectID_SendsNameAndAliasForWorkspaceConfig(t *testing.T) {
	resetFlags(t)
	dir := testfix.InitGitRepo(t)
	require.NoError(t, config.WriteProjectConfig(dir, "project-name"))

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bs, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42,"name":"project-name"}}`))
	}))
	t.Cleanup(srv.Close)

	id, err := resolveProjectID(context.Background(), srv.URL, dir)
	require.NoError(t, err)
	assert.EqualValues(t, 42, id)
	assert.Equal(t, "project-name", got["name"], "name from .kata.toml must be sent")
	alias, ok := got["alias"].(map[string]any)
	require.True(t, ok, "alias must be sent alongside name so daemon can do alias-first repair")
	assert.NotEmpty(t, alias["identity"])
	assert.NotEmpty(t, alias["kind"])
	_, hasStartPath := got["start_path"]
	assert.False(t, hasStartPath, "request must be path-free so remote daemons can resolve without stat'ing client paths")
}

// TestResolveProjectID_SendsAliasOnlyForGitWorkspaceWithoutKataToml
// covers the case where the workspace has a git root but no
// .kata.toml: the client sends alias metadata alone. The daemon must
// not derive a project name from the git remote and create-by-
// convention (resolve is strict; init owns that path).
func TestResolveProjectID_SendsAliasOnlyForGitWorkspaceWithoutKataToml(t *testing.T) {
	resetFlags(t)
	dir := testfix.InitGitRepo(t)

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bs, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42,"name":"x"}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := resolveProjectID(context.Background(), srv.URL, dir)
	require.NoError(t, err)
	alias, ok := got["alias"].(map[string]any)
	require.True(t, ok, "git workspace must yield an alias even without .kata.toml")
	assert.NotEmpty(t, alias["identity"])
	_, hasName := got["name"]
	assert.False(t, hasName, "resolve must not derive a project name from git remote (init owns by-convention)")
	_, hasStartPath := got["start_path"]
	assert.False(t, hasStartPath)
}

// TestResolveProjectID_ExplicitProjectFlagSendsNameOnly covers the
// --project override: when the caller targets a project explicitly,
// alias-first repair must not run (it could redirect away from the
// caller's chosen project). Name-only is the strict-target contract.
func TestResolveProjectID_ExplicitProjectFlagSendsNameOnly(t *testing.T) {
	resetFlags(t)
	dir := testfix.InitGitRepo(t)
	require.NoError(t, config.WriteProjectConfig(dir, "in-toml"))

	flags.Project = "explicit"

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bs, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42,"name":"explicit"}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := resolveProjectID(context.Background(), srv.URL, dir)
	require.NoError(t, err)
	assert.Equal(t, "explicit", got["name"])
	_, hasAlias := got["alias"]
	assert.False(t, hasAlias, "--project must send name only — no alias-first repair")
	_, hasStartPath := got["start_path"]
	assert.False(t, hasStartPath)
}

// TestCreate_CrossProjectLinkViaQualifiedRef pins the create-path echo and
// show rendering for cross-project link flags. A `kata create` in hub-project
// with `--blocks spoke-project#<sid>` must:
//
//  1. Succeed (exit 0).
//  2. Echo the qualified ref in the create one-liner — the synthetic echo peer
//     is fabricated by stringSliceToPeers (empty Project, ShortID =
//     "spoke-project#<sid>"), so peerRefForDisplay falls back to ShortID and
//     renders the qualified string verbatim.
//  3. `kata show` the new issue and confirm the foreign peer renders qualified
//     from wire data (daemon populates Project/QualifiedID on the response).
func TestCreate_CrossProjectLinkViaQualifiedRef(t *testing.T) {
	env := testenv.New(t)
	hubDir := initBoundWorkspace(t, env.URL, "https://github.com/example/hub-project.git")
	spokeDir := initBoundWorkspace(t, env.URL, "https://github.com/example/spoke-project.git")
	spokePID := resolvePIDViaHTTP(t, env.URL, spokeDir)

	foreignPeer := createIssue(t, env, spokePID, "foreign-peer")
	qualifiedRef := "spoke-project#" + foreignPeer

	// Step 1+2: create in hub-project with --blocks spoke-project#<sid>.
	// The create echo one-liner must contain the qualified ref.
	createOut := runCLI(t, env, hubDir, "create", "hub-subject", "--blocks", qualifiedRef)
	assert.Contains(t, createOut, qualifiedRef,
		"create one-liner must render foreign peer qualified (echo path):\n%s", createOut)

	// Decode the new issue's short_id from the quiet create response.
	subjectShortID := runCLI(t, env, hubDir, "--quiet", "create", "hub-subject2", "--blocks", qualifiedRef)
	require.NotEmpty(t, subjectShortID)

	// Step 3: kata show must render the foreign peer qualified from wire data.
	showOut := runCLI(t, env, hubDir, "show", subjectShortID)
	assert.Contains(t, showOut, qualifiedRef,
		"show output must render foreign peer qualified (wire path):\n%s", showOut)

	// Confirm the link was actually persisted: spoke issue must show blocks entry.
	spokeFetched := fetchIssueViaHTTP(t, env, spokePID, foreignPeer)
	var sawBlockedBy bool
	for _, l := range spokeFetched.Links {
		if l.Type == "blocks" && l.To.ShortID == foreignPeer {
			sawBlockedBy = true
		}
	}
	assert.True(t, sawBlockedBy, "spoke issue must have a blocks link targeting it from hub")
}

// TestResolveProjectID_RewritesStaleKataToml mirrors what the daemon
// used to do in resolveByKataToml: when the canonical project name on
// the daemon differs from the local .kata.toml (project was renamed
// daemon-side), the client rewrites the file to the canonical name.
// In remote-client mode the daemon cannot reach the client's
// filesystem, so this repair must happen on the client.
func TestResolveProjectID_RewritesStaleKataToml(t *testing.T) {
	resetFlags(t)
	dir := testfix.InitGitRepo(t)
	require.NoError(t, config.WriteProjectConfig(dir, "stale-name"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":42,"name":"canonical-name"}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := resolveProjectID(context.Background(), srv.URL, dir)
	require.NoError(t, err)

	cfg, _, err := config.FindProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "canonical-name", cfg.Project.Name,
		"stale .kata.toml must be rewritten to the daemon's canonical project name")
}
