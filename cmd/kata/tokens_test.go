package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestTokensCreateCommand_PrintsPlaintextOnce(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())

	out := requireCmdOutput(t, env, "tokens", "create", "--actor", "wesm", "--name", "laptop")

	assert.Contains(t, out, "created token id=")
	assert.Contains(t, out, "actor=wesm")
	assert.Contains(t, out, "name=laptop")
	assert.Contains(t, out, "Copy this token now. It will not be shown again.")
	assert.Contains(t, out, "Store it in the user's client config.toml or environment.")
	assert.NotContains(t, out, "token_hash")

	plaintext := extractTokenPlaintext(t, out)
	assert.Equal(t, 1, strings.Count(out, plaintext))
	resolved, err := env.DB.ResolveAPIToken(context.Background(), plaintext)
	require.NoError(t, err)
	assert.Equal(t, "wesm", resolved.Actor)
	require.NotNil(t, resolved.Name)
	assert.Equal(t, "laptop", *resolved.Name)
}

func TestTokensCreateCommand_QuietSuppressesAdvisoryText(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())

	out := requireCmdOutput(t, env, "--quiet", "tokens", "create", "--actor", "wesm")

	assert.NotContains(t, out, "Copy this token now")
	assert.NotContains(t, out, "Store it")
	plaintext := extractTokenPlaintext(t, out)
	assert.Equal(t, 1, strings.Count(out, plaintext))
}

func TestTokensCreateCommand_ExplainsIdentityModeBeforeMinting(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"))

	_, err := runCmdOutput(t, env, "tokens", "create", "--actor", "wesm")
	cliErr := requireCLIError(t, err, ExitConflict)
	assert.Equal(t, "token_identity_mode_required", cliErr.Code)
	assert.Contains(t, cliErr.Message, "require_token_identity")

	var tokens int
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM api_tokens`).Scan(&tokens))
	assert.Zero(t, tokens)
}

func TestTokensCreateCommand_WritesScopedTokenToOwnerOnlyFile(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	tokenFile := privateTokenFilePath(t)

	out := requireCmdOutput(t, env, "tokens", "create",
		"--issue", project.Name+"#"+root.ShortID,
		"--actor", "worker-a",
		"--name", "remote-worktree",
		"--expires-in", "1h",
		"--token-file", tokenFile)

	secret, err := os.ReadFile(tokenFile) //nolint:gosec // tokenFile is a test-owned temporary path.
	require.NoError(t, err)
	plaintext := strings.TrimSpace(string(secret))
	require.True(t, strings.HasPrefix(plaintext, "kata_"))
	assert.NotContains(t, out, plaintext)
	assert.NotContains(t, out, "plaintext")
	assert.Contains(t, out, "actor=worker-a")
	assert.Contains(t, out, "issue="+project.Name+"#"+root.ShortID)
	assert.Contains(t, out, "token_file="+testenv.PhysicalPath(t, tokenFile))
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(tokenFile)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	resolved, err := env.DB.ResolveAPIToken(context.Background(), plaintext)
	require.NoError(t, err)
	assert.Equal(t, "worker-a", resolved.Actor)
	require.NotNil(t, resolved.Scope)
	assert.Equal(t, db.APITokenScopeIssueSubtree, resolved.Scope.Kind)
	assert.Equal(t, project.UID, resolved.Scope.ProjectUID)
	assert.Equal(t, root.UID, resolved.Scope.RootIssueUID)
	require.NotNil(t, resolved.ExpiresAt)

	listed := requireCmdOutput(t, env, "--agent", "tokens", "list")
	assert.Contains(t, listed, "scope=issue_subtree")
	assert.Contains(t, listed, "project_uid="+project.UID)
	assert.Contains(t, listed, "root_issue_uid="+root.UID)
	assert.Contains(t, listed, "expires_at=")
}

func TestTokensCreateCommand_ScopedOutputNeverContainsPlaintext(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	for _, mode := range []struct {
		name string
		arg  string
	}{
		{name: "json", arg: "--json"},
		{name: "agent", arg: "--agent"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
			project, err := env.DB.CreateProject(context.Background(), "example-project")
			require.NoError(t, err)
			root, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
				ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
			})
			require.NoError(t, err)
			tokenFile := privateTokenFilePath(t)

			out := requireCmdOutput(t, env, mode.arg, "tokens", "create",
				"--issue", project.Name+"#"+root.ShortID,
				"--actor", "worker-a",
				"--expires-in", "1h",
				"--token-file", tokenFile)

			secret, readErr := os.ReadFile(tokenFile) //nolint:gosec // test-owned temporary path.
			require.NoError(t, readErr)
			plaintext := strings.TrimSpace(string(secret))
			assert.NotContains(t, out, plaintext)
			assert.NotContains(t, out, "plaintext")
			physicalPath := testenv.PhysicalPath(t, tokenFile)
			if mode.name == "json" {
				var result struct {
					TokenFile string `json:"token_file"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &result))
				assert.Equal(t, physicalPath, result.TokenFile)
			} else {
				plain := "token_file=" + physicalPath
				quoted := "token_file=" + strconv.Quote(physicalPath)
				assert.Truef(t, strings.Contains(out, plain) || strings.Contains(out, quoted),
					"agent output %q contains neither %q nor %q", out, plain, quoted)
			}
		})
	}
}

func TestTokensCreateCommand_ReservesScopedTokenFileBeforeMinting(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	tokenFile := privateTokenFilePath(t)
	require.NoError(t, os.WriteFile(tokenFile, []byte("keep-me"), 0o600))

	_, err = runCmdOutput(t, env, "tokens", "create",
		"--issue", project.Name+"#"+root.ShortID,
		"--actor", "worker-a",
		"--expires-in", "1h",
		"--token-file", tokenFile)
	require.Error(t, err)
	assert.ErrorContains(t, err, "already exists")
	contents, readErr := os.ReadFile(tokenFile) //nolint:gosec // test-owned temporary path.
	require.NoError(t, readErr)
	assert.Equal(t, "keep-me", string(contents))

	var count int
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM api_tokens`).Scan(&count))
	assert.Zero(t, count)
}

func TestTokensCreateCommand_RequiresCompleteScopedArguments(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	tokenFile := privateTokenFilePath(t)
	for _, args := range [][]string{
		{"tokens", "create", "--actor", "worker-a", "--issue", "example-project#abc4"},
		{"tokens", "create", "--actor", "worker-a", "--issue", "example-project#abc4", "--expires-in", "1h"},
		{"tokens", "create", "--actor", "worker-a", "--expires-in", "1h", "--token-file", tokenFile},
		{"tokens", "create", "--actor", "worker-a", "--issue", "example-project#abc4", "--expires-in", "0s", "--token-file", tokenFile},
		{"tokens", "create", "--actor", "worker-a", "--issue", "example-project#abc4", "--expires-in", "1ns", "--token-file", tokenFile},
		{"tokens", "create", "--actor", "worker-a", "--issue", "example-project#abc4", "--expires-in", "2562048h", "--token-file", tokenFile},
	} {
		_, err := runCmdOutput(t, env, args...)
		require.Error(t, err)
	}

	var count int
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM api_tokens`).Scan(&count))
	assert.Zero(t, count)
}

func TestTokensCreateCommand_RejectsConflictingExplicitProject(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	_, err = env.DB.CreateProject(context.Background(), "other-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)

	_, err = runCmdOutput(t, env,
		"--project", "other-project",
		"tokens", "create",
		"--issue", project.Name+"#"+root.ShortID,
		"--actor", "worker-a",
		"--expires-in", "1h",
		"--token-file", privateTokenFilePath(t))
	cliErr := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, "conflicting_project_selector", cliErr.Code)

	var count int
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM api_tokens`).Scan(&count))
	assert.Zero(t, count)
}

func TestTokensCreateCommand_RejectsSymlinkTokenFileWithoutTouchingTarget(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks may require elevated Windows privileges")
	}
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	dir := filepath.Dir(privateTokenFilePath(t))
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("keep-me"), 0o600))
	tokenFile := filepath.Join(dir, "worker.token")
	require.NoError(t, os.Symlink(victim, tokenFile))

	_, err = runCmdOutput(t, env, "tokens", "create",
		"--issue", project.Name+"#"+root.ShortID,
		"--actor", "worker-a",
		"--expires-in", "1h",
		"--token-file", tokenFile)
	require.Error(t, err)
	assert.ErrorContains(t, err, "already exists")
	contents, readErr := os.ReadFile(victim) //nolint:gosec // test-owned temporary path.
	require.NoError(t, readErr)
	assert.Equal(t, "keep-me", string(contents))

	var count int
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM api_tokens`).Scan(&count))
	assert.Zero(t, count)
}

func TestTokensCreateCommand_RejectsNonPrivateTokenDirectoryBeforeMinting(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	if runtime.GOOS == "windows" {
		t.Skip("Windows privacy is enforced with directory ACLs")
	}
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // Deliberately tests rejecting a public directory.
	tokenFile := filepath.Join(dir, "worker.token")

	_, err = runCmdOutput(t, env, "tokens", "create",
		"--issue", project.Name+"#"+root.ShortID,
		"--actor", "worker-a",
		"--expires-in", "1h",
		"--token-file", tokenFile)
	require.Error(t, err)
	assert.ErrorContains(t, err, "owner-only")
	_, statErr := os.Lstat(tokenFile)
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	var count int
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM api_tokens`).Scan(&count))
	assert.Zero(t, count)
}

func TestTokensCreateCommand_RejectsUnsupportedServerBeforeMinting(t *testing.T) { //nolint:paralleltest // newRootCmd resets package var flags
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "/api/v1/instance", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issue_subtree_tokens":false}`))
	}))
	t.Cleanup(server.Close)
	env := &testenv.Env{URL: server.URL}
	tokenFile := privateTokenFilePath(t)

	_, err := runCmdOutput(t, env, "tokens", "create",
		"--issue", "example-project#abc4",
		"--actor", "worker-a",
		"--expires-in", "1h",
		"--token-file", tokenFile)
	cliErr := requireCLIError(t, err, ExitConflict)
	assert.Equal(t, "issue_subtree_tokens_unsupported", cliErr.Code)
	assert.Equal(t, 1, requests)
	_, statErr := os.Lstat(tokenFile)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestTokensCreateCommand_RevokesMismatchedScopedResponseWithoutWritingSecret(t *testing.T) { //nolint:paralleltest // newRootCmd resets package var flags
	const secret = "kata_mismatched-secret"
	revoked := false
	server := scopedTokenTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"plaintext":%q,"token":{"id":7,"actor":"different-worker","scope":{"kind":"issue_subtree","project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","root_issue_uid":"01HZNQ7VFPK1XGD8R5MABCD5YZ"},"created_at":"2026-09-15T10:00:00Z","expires_at":"2026-09-15T11:00:00Z"}}`, secret)
		case "/api/v1/tokens/7/actions/revoke":
			revoked = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":{"id":7,"actor":"different-worker"}}`))
		default:
			http.NotFound(w, r)
		}
	})
	t.Cleanup(server.Close)
	tokenFile := privateTokenFilePath(t)

	out, err := runCmdOutput(t, &testenv.Env{URL: server.URL}, "tokens", "create",
		"--issue", "example-project#abc4",
		"--actor", "worker-a",
		"--expires-in", "1h",
		"--token-file", tokenFile)
	require.Error(t, err)
	assert.ErrorContains(t, err, "was revoked")
	assert.NotContains(t, out, secret)
	assert.NotContains(t, err.Error(), secret)
	assert.True(t, revoked)
	_, statErr := os.Lstat(tokenFile)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestTokensCreateCommand_ReportsAmbiguousUndecodableResponse(t *testing.T) { //nolint:paralleltest // newRootCmd resets package var flags
	server := scopedTokenTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tokens" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plaintext":`))
	})
	t.Cleanup(server.Close)
	tokenFile := privateTokenFilePath(t)

	_, err := runCmdOutput(t, &testenv.Env{URL: server.URL}, "tokens", "create",
		"--issue", "example-project#abc4",
		"--actor", "worker-a",
		"--expires-in", "1h",
		"--token-file", tokenFile)
	require.Error(t, err)
	assert.ErrorContains(t, err, "outcome is ambiguous")
	_, statErr := os.Lstat(tokenFile)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestCleanupScopedTokenCreationSurvivesCanceledCommandContext(t *testing.T) {
	t.Parallel()
	revoked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		revoked = true
		_, _ = w.Write([]byte(`{"token":{"id":7,"actor":"worker-a"}}`))
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := cleanupScopedTokenCreation(daemonAPI{
		ctx: ctx, baseURL: server.URL, client: server.Client(),
	}, 7, errors.New("delivery failed"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "was revoked")
	require.True(t, revoked)
}

func TestTokensListCommand_RedactsPlaintextAndHash(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	createOut := requireCmdOutput(t, env, "tokens", "create", "--actor", "wesm", "--name", "laptop")
	plaintext := extractTokenPlaintext(t, createOut)

	out := requireCmdOutput(t, env, "tokens", "list")

	assert.Contains(t, out, "id  actor  name")
	assert.Contains(t, out, "wesm")
	assert.Contains(t, out, "laptop")
	assert.NotContains(t, out, plaintext)
	assert.NotContains(t, out, "token_hash")
}

func TestTokensRevokeCommand_RevokesToken(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	tok, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "user-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "tokens", "revoke", strconv.FormatInt(tok.ID, 10))

	assert.Contains(t, out, "revoked token id="+strconv.FormatInt(tok.ID, 10))
	assert.Contains(t, out, "actor=alice")
	_, err = env.DB.ResolveAPIToken(context.Background(), "user-token")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestTokensCommands_JSONAndAgentOutput(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())

	jsonOut := requireCmdOutput(t, env, "--json", "tokens", "create", "--actor", "wesm")
	var created struct {
		APIVersion int `json:"kata_api_version"`
		Token      struct {
			ID    int64  `json:"id"`
			Actor string `json:"actor"`
		} `json:"token"`
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &created))
	assert.Equal(t, 1, created.APIVersion)
	assert.Equal(t, "wesm", created.Token.Actor)
	assert.True(t, strings.HasPrefix(created.Plaintext, "kata_"))
	assert.Equal(t, 1, strings.Count(jsonOut, created.Plaintext))
	assert.NotContains(t, jsonOut, "token_hash")

	agentOut := requireCmdOutput(t, env, "--agent", "tokens", "list")
	assert.Contains(t, agentOut, "OK tokens count=1\n")
	assert.Contains(t, agentOut, "- id="+strconv.FormatInt(created.Token.ID, 10)+" actor=wesm")
	assert.NotContains(t, agentOut, created.Plaintext)
	assert.NotContains(t, agentOut, "token_hash")
}

func extractTokenPlaintext(t *testing.T, out string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if token, ok := strings.CutPrefix(line, "token="); ok {
			require.NotEmpty(t, token)
			require.True(t, strings.HasPrefix(token, "kata_"), "plaintext line %q", line)
			return token
		}
	}
	t.Fatalf("missing token= line in output:\n%s", out)
	return ""
}

func privateTokenFilePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // Directory mode, not a secret file.
	}
	return filepath.Join(dir, "worker.token")
}

func scopedTokenTestServer(t *testing.T, next http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/instance":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issue_subtree_tokens":true}`))
		case "/api/v1/projects/resolve":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"project":{"id":42,"uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","name":"example-project"}}`))
		case "/api/v1/projects/42/issues/abc4":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issue":{"uid":"01HZNQ7VFPK1XGD8R5MABCD5YZ","project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","short_id":"abc4"}}`))
		default:
			next(w, r)
		}
	}))
}
