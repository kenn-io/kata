package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// Intercept before TestMain: run the real CLI with only the parent-provided
// synthetic home/environment. This is not an AgentsView push impersonator.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "kata-agentsview-fixture-cli" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		main()
		os.Exit(0)
	}
}

func agentsViewSyntheticProcessEnv(home, server string) []string {
	environment := []string{}
	for _, name := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TMPDIR", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	// No account-home overrides, provider credentials, proxy, hosted PORT,
	// daemon catalog/server overrides or archive roots cross the child boundary.
	return append(environment,
		"HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_STATE_HOME="+filepath.Join(home, "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"), "KATA_HOME="+filepath.Join(home, "kata"),
		"AGENTSVIEW_DATA_DIR="+filepath.Join(home, "archive"), "KATA_SERVER="+server,
		"KATA_TRANSCRIPT_AGENT=pi", "KATA_TRANSCRIPT_SESSION_ID=selected", "DO_NOT_TRACK=1",
	)
}

func TestAgentsViewUnavailableProcessSendsNoBytesToEitherDestination(t *testing.T) {
	var company, personal atomic.Int64
	companyReceiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { company.Add(1); w.WriteHeader(500) }))
	defer companyReceiver.Close()
	personalReceiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { personal.Add(1); w.WriteHeader(500) }))
	defer personalReceiver.Close()
	_, _, consent, snapshot := agentsViewWorkflowFixture(t)
	raw, err := json.Marshal(consent)
	require.NoError(t, err)
	consentPath := filepath.Join(t.TempDir(), "consent.json")
	require.NoError(t, os.WriteFile(consentPath, raw, 0o600))
	for _, destination := range []string{companyReceiver.URL, personalReceiver.URL} {
		home := t.TempDir()
		process := exec.CommandContext(t.Context(), os.Args[0], "kata-agentsview-fixture-cli", "agentsview", "share", consentPath, "--issue", "example#abcd", "--snapshot", snapshot, "--close-event", "close-a", "--confirm-transcript-sha256", consent.Association.SnapshotSHA256) //nolint:gosec // fixed test executable and synthetic explicit fixture inputs
		process.Dir = home
		process.Env = agentsViewSyntheticProcessEnv(home, destination)
		out, err := process.CombinedOutput()
		require.Error(t, err)
		require.Contains(t, string(out), "sharing is unavailable")
		require.Zero(t, company.Load())
		require.Zero(t, personal.Load())
	}
}

func TestAgentsViewSyntheticProcessEnvironmentOmitsAuthority(t *testing.T) {
	t.Setenv("CODEX_HOME", "private-account")
	t.Setenv("CLAUDE_CONFIG_DIR", "private-account")
	t.Setenv("PI_CODING_AGENT_DIR", "private-account")
	t.Setenv("HTTP_PROXY", "private-proxy")
	t.Setenv("KATA_AUTH_TOKEN", "private-token")
	t.Setenv("AGENTSVIEW_PUSH_TOKEN", "private-token")
	t.Setenv("PORT", "7777")
	environment := strings.Join(agentsViewSyntheticProcessEnv(t.TempDir(), "http://127.0.0.1:12345"), "\n")
	require.NotContains(t, environment, "private-account")
	require.NotContains(t, environment, "private-token")
	require.NotContains(t, environment, "private-proxy")
	require.NotContains(t, environment, "PORT=")
}
