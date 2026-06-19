//go:build !windows

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

const identityBootstrapToken = "identity-bootstrap-token"

func TestTokenIdentity_OverridesClientActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(context.Background(), "identity-e2e")
	require.NoError(t, err)

	createTokenResp := doJSONWithBearer(t, env.HTTP, http.MethodPost, env.URL+"/api/v1/tokens",
		"bootstrap-token", map[string]any{"actor": "alice", "name": "laptop"})
	require.Equal(t, http.StatusOK, createTokenResp.status, createTokenResp.body)
	var tokenOut struct {
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal([]byte(createTokenResp.body), &tokenOut))
	require.NotEmpty(t, tokenOut.Plaintext)

	createIssueResp := doJSONWithBearer(t, env.HTTP, http.MethodPost,
		env.URL+"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues",
		tokenOut.Plaintext, map[string]any{"actor": "mallory", "title": "identity override"})
	require.Equal(t, http.StatusOK, createIssueResp.status, createIssueResp.body)
	var mutation struct {
		Event struct {
			Actor string `json:"actor"`
		} `json:"event"`
		Issue struct {
			Author string `json:"author"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(createIssueResp.body), &mutation))
	assert.Equal(t, "alice", mutation.Event.Actor)
	assert.Equal(t, "alice", mutation.Issue.Author)
}

func TestTokenIdentity_RemoteCLIUsesUserTokenActor(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	bin := buildKataBinary(t)
	addr, serverHome, stop := startIdentityDaemon(t, bin)
	defer stop()

	adminEnv := identityCLIEnv(serverHome, addr, identityBootstrapToken)
	userToken := createIdentityTokenCLI(t, bin, adminEnv, "alice")

	clientHome := t.TempDir()
	clientWS := initRepo(t, "https://example.invalid/team/identity-cli.git")
	clientEnv := identityCLIEnv(clientHome, addr, userToken)

	runRemoteCmd(t, bin, clientWS, clientEnv,
		"--project", "identity-cli", "init")
	runRemoteCmd(t, bin, clientWS, clientEnv,
		"--as", "mallory", "create", "token attributed issue")

	out := runRemoteCmdOutput(t, bin, clientWS, clientEnv,
		"list", "--json")
	var listed struct {
		Issues []struct {
			ShortID string `json:"short_id"`
			Author  string `json:"author"`
		} `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed),
		"list --json should decode: %s", out)
	require.Len(t, listed.Issues, 1, "expected exactly one issue: %s", out)
	assert.Equal(t, "alice", listed.Issues[0].Author)
	assert.NotEqual(t, "mallory", listed.Issues[0].Author)
}

func TestTokenIdentity_BootstrapCanResolveButCannotWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	bin := buildKataBinary(t)
	addr, serverHome, stop := startIdentityDaemon(t, bin)
	defer stop()

	adminEnv := identityCLIEnv(serverHome, addr, identityBootstrapToken)
	userToken := createIdentityTokenCLI(t, bin, adminEnv, "alice")

	clientHome := t.TempDir()
	clientWS := initRepo(t, "https://example.invalid/team/bootstrap-boundary.git")
	userEnv := identityCLIEnv(clientHome, addr, userToken)
	runRemoteCmd(t, bin, clientWS, userEnv,
		"--project", "bootstrap-boundary", "init")

	bootstrapEnv := identityCLIEnv(clientHome, addr, identityBootstrapToken)
	runRemoteCmd(t, bin, clientWS, bootstrapEnv, "projects", "show", "bootstrap-boundary")

	resolveWithAlias := doJSONWithBearer(t, http.DefaultClient, http.MethodPost,
		"http://"+addr+"/api/v1/projects/resolve", identityBootstrapToken,
		map[string]any{
			"name": "bootstrap-boundary",
			"alias": map[string]any{
				"identity": "example.invalid/team/bootstrap-boundary",
				"kind":     "git",
			},
		})
	require.Equal(t, http.StatusForbidden, resolveWithAlias.status, resolveWithAlias.body)
	assert.Contains(t, resolveWithAlias.body, "bootstrap_token_write_forbidden")

	resolveByName := doJSONWithBearer(t, http.DefaultClient, http.MethodPost,
		"http://"+addr+"/api/v1/projects/resolve", identityBootstrapToken,
		map[string]any{"name": "bootstrap-boundary"})
	require.Equal(t, http.StatusOK, resolveByName.status, resolveByName.body)

	out, err := runRemoteCmdOutputErr(t, bin, clientWS, bootstrapEnv,
		"create", "bootstrap should not write")
	require.Error(t, err, "bootstrap token must not perform attributed writes")
	assert.Contains(t, out, "bootstrap token cannot perform attributed writes")

	listOut := runRemoteCmdOutput(t, bin, clientWS, userEnv, "list", "--json")
	assert.NotContains(t, listOut, "bootstrap should not write")
}

func TestTokenIdentity_FederationPersonalTokenEnrollJoinAndPush(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	ctx := context.Background()
	const projectName = "identity-federation-e2e"
	const pushedTitle = "spoke issue from token-bound federation"

	bin := buildKataBinary(t)
	hubAddr, hubHome, stopHub := startIdentityDaemon(t, bin)
	defer stopHub()
	hubDB, err := sqlitestore.Open(ctx, filepath.Join(hubHome, "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = hubDB.Close() })

	adminEnv := identityCLIEnv(hubHome, hubAddr, identityBootstrapToken)
	userToken := createIdentityTokenCLI(t, bin, adminEnv, "wesm")
	hubClientHome := t.TempDir()
	hubEnv := identityCLIEnv(hubClientHome, hubAddr, userToken)
	hubWS := initRepo(t, "https://example.invalid/team/identity-federation-hub.git")
	runRemoteCmd(t, bin, hubWS, hubEnv, "--project", projectName, "init")

	spokeDirs := newE2EDirs(t)
	spokeEnv := append(spokeDirs.env(),
		"KATA_AUTH_TOKEN=",
		"KATA_FEDERATION_PULL_INTERVAL_MS=25",
		"KATA_HTTP_TIMEOUT=10s",
	)
	spokeStderr := startDaemon(t, bin, spokeEnv)
	_, _ = connectDaemon(t, spokeDirs, spokeStderr)
	spokeIdentityOut := runRemoteCmdOutput(t, bin, spokeDirs.repoDir, spokeEnv,
		"--json", "federation", "identity")
	var spokeIdentity struct {
		InstanceUID string `json:"instance_uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(spokeIdentityOut), &spokeIdentity),
		"spoke identity output: %s", spokeIdentityOut)
	require.NotEmpty(t, spokeIdentity.InstanceUID)

	bootstrapEnrollEnv := append(spokeDirs.env(),
		"KATA_AUTH_TOKEN="+identityBootstrapToken,
		"KATA_AUTHOR=e2e-client",
		"KATA_HTTP_TIMEOUT=10s",
	)
	bootstrapOut, bootstrapErr := runRemoteCmdOutputErr(t, bin, spokeDirs.repoDir, bootstrapEnrollEnv,
		"federation", "enroll", projectName,
		"--spoke-instance", spokeIdentity.InstanceUID,
		"--hub-url", "http://"+hubAddr,
		"--actor", "wesm")
	require.Error(t, bootstrapErr, "bootstrap token must not create identity-mode federation enrollments")
	assert.Contains(t, bootstrapOut, "bootstrap token cannot perform attributed writes")

	enrollEnv := append(spokeDirs.env(),
		"KATA_AUTH_TOKEN="+userToken,
		"KATA_AUTHOR=e2e-client",
		"KATA_HTTP_TIMEOUT=10s",
	)
	enrollOut := runRemoteCmdOutput(t, bin, spokeDirs.repoDir, enrollEnv,
		"federation", "enroll", projectName,
		"--spoke-instance", spokeIdentity.InstanceUID,
		"--hub-url", "http://"+hubAddr,
		"--capabilities", "pull,push,lease",
		"--actor", "mallory")
	joinCommand := extractFederationJoinCommand(t, enrollOut)
	assert.Contains(t, joinCommand, "--actor wesm")
	assert.NotContains(t, joinCommand, "mallory")

	spokeShellEnv := append(spokeEnv, "PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	joinOut, err := runShellOutput(spokeDirs.repoDir, spokeShellEnv, joinCommand)
	require.NoErrorf(t, err, "join command %q failed:\n%s", joinCommand, joinOut)
	assert.Contains(t, joinOut, "joined federation project "+projectName)
	assert.Contains(t, joinOut, "push-enabled: true")

	runRemoteCmd(t, bin, spokeDirs.repoDir, spokeEnv,
		"--project", projectName,
		"--as", "mallory",
		"create", pushedTitle)

	hubProject, err := hubDB.ProjectByName(ctx, projectName)
	require.NoError(t, err)
	pushed := waitForFederatedTitle(t, hubDB, pushedTitle, &safeBuffer{}, 10*time.Second)
	assert.Equal(t, "wesm", pushed.Author)
	assert.NotEqual(t, "mallory", pushed.Author)

	events, err := hubDB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: hubProject.ID, Limit: 100})
	require.NoError(t, err)
	var createdEvent *db.Event
	for i := range events {
		if events[i].Type == "issue.created" && events[i].IssueUID != nil && *events[i].IssueUID == pushed.UID {
			createdEvent = &events[i]
			break
		}
	}
	require.NotNil(t, createdEvent, "hub did not receive issue.created for %s", pushed.UID)
	assert.Equal(t, "wesm", createdEvent.Actor)

	hubList := runRemoteCmdOutput(t, bin, hubWS, hubEnv,
		"--project", projectName, "list", "--json")
	assert.Contains(t, hubList, `"author":"wesm"`)
	assert.NotContains(t, hubList, "mallory")
}

type rawHTTPResponse struct {
	status int
	body   string
}

func doJSONWithBearer(t *testing.T, client *http.Client, method, url, bearer string, body any) rawHTTPResponse {
	t.Helper()
	bs, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), method, url, bytes.NewReader(bs))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req) //nolint:gosec // test-only loopback URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return rawHTTPResponse{status: resp.StatusCode, body: drain(t, resp)}
}

func startIdentityDaemon(t *testing.T, bin string) (addr, serverHome string, stop func()) {
	t.Helper()
	port := freeTCPPort(t)
	addr = fmt.Sprintf("127.0.0.1:%d", port)
	serverHome = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(serverHome, "config.toml"),
		[]byte("[auth]\ntoken = \""+identityBootstrapToken+"\"\nrequire_token_identity = true\n"), 0o600))

	stderr := &safeBuffer{}
	cmd := exec.Command(bin, "daemon", "start", "--listen", addr) //nolint:gosec
	cmd.Env = append(os.Environ(),
		"KATA_HOME="+serverHome,
		"KATA_DB="+filepath.Join(serverHome, "kata.db"),
		"KATA_AUTH_TOKEN=",
		"KATA_AUTOSTART=",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		stopDaemon(cmd)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon stderr:\n%s", stderr.String())
		}
		stop()
	})

	waitForPing(t, "http://"+addr, 5*time.Second)
	return addr, serverHome, stop
}

func identityCLIEnv(home, addr, token string) []string {
	return append(os.Environ(),
		"KATA_HOME="+home,
		"KATA_DB="+filepath.Join(home, "kata.db"),
		"KATA_SERVER=http://"+addr,
		"KATA_AUTH_TOKEN="+token,
		"KATA_AUTHOR=e2e-client",
	)
}

func createIdentityTokenCLI(t *testing.T, bin string, env []string, actor string) string {
	t.Helper()
	out := runRemoteCmdOutput(t, bin, "", env,
		"tokens", "create", "--actor", actor, "--name", "e2e")
	for _, line := range strings.Split(out, "\n") {
		token, ok := strings.CutPrefix(line, "token=")
		if ok {
			require.NotEmpty(t, token)
			return token
		}
	}
	t.Fatalf("missing token= line in output:\n%s", out)
	return ""
}

func runRemoteCmdOutputErr(t *testing.T, bin, workdir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec
	cmd.Dir = workdir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func extractFederationJoinCommand(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if cmd, ok := strings.CutPrefix(line, "join: "); ok {
			require.NotEmpty(t, strings.TrimSpace(cmd), "empty join command in output:\n%s", out)
			return strings.TrimSpace(cmd)
		}
	}
	t.Fatalf("missing join command in output:\n%s", out)
	return ""
}

func runShellOutput(workdir string, env []string, command string) (string, error) {
	cmd := exec.Command("sh", "-c", command) //nolint:gosec // test executes a kata-generated join command.
	cmd.Dir = workdir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}
