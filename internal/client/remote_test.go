package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pingingServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ping" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"service": "kata",
			"version": "test",
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func TestResolveRemote_NoEnvNoFile(t *testing.T) {
	t.Setenv("KATA_SERVER", "")
	dir := t.TempDir()
	t.Chdir(dir)

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, url)
}

func TestResolveRemote_EnvWinsAndProbes(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", srv.URL)

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, srv.URL, url)
}

func TestResolveRemote_EnvUnreachableErrors(t *testing.T) {
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1") // closed port

	_, _, err := resolveRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KATA_SERVER")
	assert.Contains(t, err.Error(), "http://127.0.0.1:1")
	assert.ErrorIs(t, err, ErrRemoteUnavailable)
}

func TestResolveRemote_EnvMalformedErrors(t *testing.T) {
	t.Setenv("KATA_SERVER", "::not-a-url::")

	_, _, err := resolveRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KATA_SERVER")
}

func TestResolveRemote_FileWhenNoEnv(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", "")
	dir := t.TempDir()
	t.Chdir(dir)
	writeWorkspaceMarker(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "`+srv.URL+`"
`), 0o600))

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, srv.URL, url)
}

func TestResolveRemote_EnvWinsOverFile(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", srv.URL)
	dir := t.TempDir()
	t.Chdir(dir)
	writeWorkspaceMarker(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "http://10.255.255.1:9"
`), 0o600))

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, srv.URL, url, "env URL must win over file URL")
}

func TestResolveRemote_FileEmptyURLFallsThrough(t *testing.T) {
	t.Setenv("KATA_SERVER", "")
	dir := t.TempDir()
	t.Chdir(dir)
	writeWorkspaceMarker(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = ""
`), 0o600))

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.False(t, ok, "empty server URL must be treated as no remote configured")
	assert.Empty(t, url)
}

func TestResolveRemote_FileUnreachableErrors(t *testing.T) {
	t.Setenv("KATA_SERVER", "")
	dir := t.TempDir()
	t.Chdir(dir)
	writeWorkspaceMarker(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "http://127.0.0.1:1"
`), 0o600))

	_, _, err := resolveRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".kata.local.toml")
	assert.ErrorIs(t, err, ErrRemoteUnavailable)
}

func TestResolveRemote_FileFoundInParentDirectory(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", "")
	parent := t.TempDir()
	child := filepath.Join(parent, "subdir")
	require.NoError(t, os.Mkdir(child, 0o755)) //nolint:gosec // test fixture under TempDir
	writeWorkspaceMarker(t, parent)
	require.NoError(t, os.WriteFile(filepath.Join(parent, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "`+srv.URL+`"
`), 0o600))
	t.Chdir(child)

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, srv.URL, url)
}

// TestResolveRemote_WorkspaceAnchorOverridesCwd guards finding #7:
// when the user runs `kata --workspace /some/repo create` from
// outside that repo, the .kata.local.toml at the workspace root must
// be discovered. Without the workspaceStart argument, the walk would
// start at CWD and miss it.
func TestResolveRemote_WorkspaceAnchorOverridesCwd(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", "")
	cwd := t.TempDir()
	workspace := t.TempDir()
	t.Chdir(cwd)
	writeWorkspaceMarker(t, workspace)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "`+srv.URL+`"
`), 0o600))

	url, ok, err := resolveRemote(context.Background(), workspace)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, srv.URL, url, "must walk from workspaceStart, not CWD")
}

// TestResolveRemote_EmptyWorkspaceFallsBackToCwd preserves the
// existing default behavior: when no --workspace is set, the walk
// still begins at CWD.
func TestResolveRemote_EmptyWorkspaceFallsBackToCwd(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", "")
	dir := t.TempDir()
	t.Chdir(dir)
	writeWorkspaceMarker(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "`+srv.URL+`"
`), 0o600))

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, srv.URL, url)
}

// TestResolveRemote_FileIgnoredWithoutWorkspaceMarker covers the
// security finding from roborev #18-? : a .kata.local.toml in a
// shared/world-writable ancestor (e.g. /tmp) must not be honored when
// the user is running kata outside any workspace. Without the
// boundary check, an attacker-placed config could route a victim's
// requests to an arbitrary URL.
func TestResolveRemote_FileIgnoredWithoutWorkspaceMarker(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", "")
	// CWD is a fresh tempdir with no .kata.toml / .git anywhere up
	// the chain we care about. An attacker-style .kata.local.toml
	// lives there. With no workspace anchor, it must be ignored.
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "`+srv.URL+`"
`), 0o600))

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.False(t, ok, ".kata.local.toml without a workspace marker must not be honored")
	assert.Empty(t, url)
}

// TestResolveRemote_FileIgnoredAboveWorkspaceBoundary covers the
// concrete attack: attacker plants .kata.local.toml in a shared
// ancestor; victim's workspace sits below it with its own
// .kata.toml. The walk must stop at the workspace root and never see
// the attacker file.
func TestResolveRemote_FileIgnoredAboveWorkspaceBoundary(t *testing.T) {
	t.Setenv("KATA_SERVER", "")
	outer := t.TempDir()
	workspace := filepath.Join(outer, "workspace")
	sub := filepath.Join(workspace, "deep", "subdir")
	require.NoError(t, os.MkdirAll(sub, 0o755)) //nolint:gosec // test fixture under TempDir
	writeWorkspaceMarker(t, workspace)

	// Attacker file in the shared ancestor — points at an unreachable
	// address so that if the test ever started honoring it, we'd see
	// an ErrRemoteUnavailable rather than a silent pass.
	require.NoError(t, os.WriteFile(filepath.Join(outer, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "http://127.0.0.1:1"
`), 0o600))
	t.Chdir(sub)

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.False(t, ok, "walk must stop at the workspace boundary, never reach outer/.kata.local.toml")
	assert.Empty(t, url)
}

// TestResolveRemote_FileInsideWorkspaceWinsOverOutsideAncestor
// confirms the legitimate flow still works: a workspace-local
// .kata.local.toml is honored even when an unrelated file exists in
// a higher shared ancestor.
func TestResolveRemote_FileInsideWorkspaceWinsOverOutsideAncestor(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", "")
	outer := t.TempDir()
	workspace := filepath.Join(outer, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755)) //nolint:gosec // test fixture under TempDir
	writeWorkspaceMarker(t, workspace)

	// Attacker file higher up — must be ignored.
	require.NoError(t, os.WriteFile(filepath.Join(outer, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "http://127.0.0.1:1"
`), 0o600))
	// Legitimate file at workspace root — must be honored.
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "`+srv.URL+`"
`), 0o600))
	t.Chdir(workspace)

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, srv.URL, url, "workspace-local config must win, outer config must never be reached")
}

// TestResolveRemote_GitMarkerCountsAsWorkspace allows pre-init flows:
// a freshly cloned repo has .git but not yet .kata.toml; a developer
// can still drop a .kata.local.toml beside .git to point at a remote
// daemon for the upcoming `kata init`.
func TestResolveRemote_GitMarkerCountsAsWorkspace(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", "")
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755)) //nolint:gosec // test fixture under TempDir
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "`+srv.URL+`"
`), 0o600))
	t.Chdir(dir)

	url, ok, err := resolveRemote(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, srv.URL, url)
}

// TestNormalizeRemoteURL_SchemeGuard covers the plain-http guard.
// Plain http is rejected for public IPs and hostnames; loopback and
// private IPs are accepted; https and allow_insecure short-circuit.
func TestNormalizeRemoteURL_SchemeGuard(t *testing.T) {
	cases := []struct {
		name           string
		url            string
		allowInsecure  bool
		wantOK         bool
		wantErrSubstr  string
		wantNormalized string
	}{
		{
			name: "https public host allowed",
			url:  "https://example.com:7777", wantOK: true,
			wantNormalized: "https://example.com:7777",
		},
		{
			name: "http loopback allowed",
			url:  "http://127.0.0.1:7777", wantOK: true,
			wantNormalized: "http://127.0.0.1:7777",
		},
		{
			name: "http rfc1918 allowed",
			url:  "http://10.0.0.5:7777", wantOK: true,
			wantNormalized: "http://10.0.0.5:7777",
		},
		{
			name: "http cgnat allowed (tailscale range)",
			url:  "http://100.64.0.5:7777", wantOK: true,
			wantNormalized: "http://100.64.0.5:7777",
		},
		{
			name: "http public ipv4 rejected",
			url:  "http://8.8.8.8:7777", wantOK: false,
			wantErrSubstr: "plain http to \"8.8.8.8\"",
		},
		{
			name: "http hostname rejected (cannot validate without DNS)",
			url:  "http://kata.example.com:7777", wantOK: false,
			wantErrSubstr: "plain http to \"kata.example.com\"",
		},
		{
			name: "http localhost hostname rejected (use 127.0.0.1)",
			url:  "http://localhost:7777", wantOK: false,
			wantErrSubstr: "plain http to \"localhost\"",
		},
		{
			name: "http public ipv4 allowed when allow_insecure",
			url:  "http://8.8.8.8:7777", allowInsecure: true, wantOK: true,
			wantNormalized: "http://8.8.8.8:7777",
		},
		{
			name: "http hostname allowed when allow_insecure",
			url:  "http://kata.example.com:7777", allowInsecure: true, wantOK: true,
			wantNormalized: "http://kata.example.com:7777",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeRemoteURL(tc.url, tc.allowInsecure)
			if tc.wantOK {
				require.NoError(t, err)
				assert.Equal(t, tc.wantNormalized, got)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrSubstr)
		})
	}
}

// TestResolveRemote_EnvSchemeGuardRejectsPublicHTTP verifies the guard
// fires through the env-driven entry point with a clear actionable
// error mentioning KATA_ALLOW_INSECURE.
func TestResolveRemote_EnvSchemeGuardRejectsPublicHTTP(t *testing.T) {
	t.Setenv("KATA_SERVER", "http://8.8.8.8:7777")
	t.Setenv("KATA_ALLOW_INSECURE", "")

	_, _, err := resolveRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KATA_SERVER")
	assert.Contains(t, err.Error(), "allow_insecure")
}

// TestResolveRemote_EnvAllowInsecureBypassesGuard confirms the env
// opt-out lets a public-http URL through the guard. The probe is
// stubbed to return false so the surface error is
// ErrRemoteUnavailable; this avoids any real outbound dial.
func TestResolveRemote_EnvAllowInsecureBypassesGuard(t *testing.T) {
	stubProbe(t, false)
	t.Setenv("KATA_SERVER", "http://198.51.100.1:7777") // TEST-NET-2, never dialed
	t.Setenv("KATA_ALLOW_INSECURE", "1")

	_, _, err := resolveRemote(context.Background(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRemoteUnavailable)
	assert.NotContains(t, err.Error(), "allow_insecure",
		"guard message must not appear when allow_insecure is set")
}

// TestResolveRemote_FileSchemeGuardRejectsPublicHTTP exercises the
// guard via .kata.local.toml. Without allow_insecure the URL is
// rejected before the probe runs.
func TestResolveRemote_FileSchemeGuardRejectsPublicHTTP(t *testing.T) {
	t.Setenv("KATA_SERVER", "")
	dir := t.TempDir()
	t.Chdir(dir)
	writeWorkspaceMarker(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "http://8.8.8.8:7777"
`), 0o600))

	_, _, err := resolveRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".kata.local.toml")
	assert.Contains(t, err.Error(), "allow_insecure")
}

// TestResolveRemote_FileAllowInsecureBypassesGuard confirms the file
// opt-out lets a public-http URL through the guard. The probe is
// stubbed to return false so the surface error is
// ErrRemoteUnavailable; this avoids any real outbound dial.
func TestResolveRemote_FileAllowInsecureBypassesGuard(t *testing.T) {
	stubProbe(t, false)
	t.Setenv("KATA_SERVER", "")
	dir := t.TempDir()
	t.Chdir(dir)
	writeWorkspaceMarker(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"),
		[]byte(`version = 1
[server]
url = "http://198.51.100.1:7777"
allow_insecure = true
`), 0o600))

	_, _, err := resolveRemote(context.Background(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRemoteUnavailable)
	assert.NotContains(t, err.Error(), "is not allowed",
		"guard message must not appear when allow_insecure is set")
}

// TestEnvAllowInsecure_Truthiness covers the case-insensitive parsing
// of KATA_ALLOW_INSECURE. Empty / "0" / "false" must be false; "1" and
// "true" in any case must be true; surrounding whitespace is trimmed.
func TestEnvAllowInsecure_Truthiness(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"yes", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"tRuE", true},
		{" 1 ", true},
		{"  true\t", true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("val=%q", tc.val), func(t *testing.T) {
			t.Setenv("KATA_ALLOW_INSECURE", tc.val)
			assert.Equal(t, tc.want, envAllowInsecure())
		})
	}
}

// stubProbe replaces probeRemote for the duration of the test so
// resolution paths past the guard don't issue real outbound dials.
// Restored via t.Cleanup.
func stubProbe(t *testing.T, ok bool) {
	t.Helper()
	saved := probeRemote
	probeRemote = func(_ context.Context, _ string) bool { return ok }
	t.Cleanup(func() { probeRemote = saved })
}

// writeWorkspaceMarker drops a minimal .kata.toml at dir so the
// test mimics a real kata workspace, anchoring .kata.local.toml
// discovery to a legitimate boundary.
func writeWorkspaceMarker(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), //nolint:gosec // test fixture mirrors production .kata.toml mode
		[]byte("version = 1\n\n[project]\nidentity = \"github.com/wesm/test\"\nname = \"test\"\n"),
		0o644))
}
