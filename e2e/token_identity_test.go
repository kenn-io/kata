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
	clientWS := initRepo(t, "https://github.com/wesm/identity-cli.git")
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
	clientWS := initRepo(t, "https://github.com/wesm/bootstrap-boundary.git")
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
				"identity":  "github.com/wesm/bootstrap-boundary",
				"kind":      "git",
				"root_path": clientWS,
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
