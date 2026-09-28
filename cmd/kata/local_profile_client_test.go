package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestLocalProfileSearchKeepsSelectedCredential(t *testing.T) {
	resetFlags(t)
	personalHome, workHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("KATA_HOME", personalHome)
	t.Setenv("KATA_AUTH_TOKEN", "personal-token")
	t.Setenv("KATA_SERVER", "")
	store, err := sqlitestore.Open(t.Context(), filepath.Join(workHome, "kata.db"))
	require.NoError(t, err)
	uid := store.InstanceUID()
	require.NoError(t, store.Close())
	require.NoError(t, os.WriteFile(filepath.Join(workHome, "config.toml"), []byte("[auth]\ntoken = \"work-token\"\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(personalHome, "config.toml"), []byte(fmt.Sprintf(`[[daemon]]
name = "work"
local = true
home = %q
instance_uid = %q
`, workHome, uid)), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.toml"), []byte("version = 1\n[project]\nname = \"spoke-project\"\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.local.toml"), []byte("version = 1\n[server]\ndaemon = \"work\"\n"), 0600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"test"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer work-token" {
			http.Error(w, "wrong profile credential", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/instance":
			_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
		case "/api/v1/projects/resolve":
			_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"}}`)
		case "/api/v1/projects/1/search":
			_, _ = fmt.Fprint(w, `{"query":"example","mode":"lexical","results":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: workHome, InstanceUID: uid})
	require.NoError(t, err)
	ns, err := daemon.NewNamespaceForHome(workHome, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"})
	require.NoError(t, err)
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "1")
	flags.Workspace = root
	var out bytes.Buffer
	cmd := newSearchCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"example"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
}

func TestLocalProfileDestructiveCommandsUseSelectedCredential(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verb     string
		confirm  string
		response string
	}{
		{name: "delete", verb: "delete", confirm: "DELETE spoke-project#abc4", response: `{"issue":{"short_id":"abc4","title":"fixture"},"changed":true}`},
		{name: "purge", verb: "purge", confirm: "PURGE spoke-project#abc4", response: `{"purge_log":{"short_id":"abc4","issue_title":"fixture"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			personalHome, workHome, root := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("KATA_HOME", personalHome)
			t.Setenv("KATA_DB", filepath.Join(personalHome, "kata.db"))
			t.Setenv("KATA_DSN", "")
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_AUTH_TOKEN", "personal-token")
			t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "1")

			store, err := sqlitestore.Open(t.Context(), filepath.Join(workHome, "kata.db"))
			require.NoError(t, err)
			uid := store.InstanceUID()
			require.NoError(t, store.Close())
			require.NoError(t, os.WriteFile(filepath.Join(workHome, "config.toml"), []byte("[auth]\ntoken = \"work-token\"\n"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(personalHome, "config.toml"), []byte(fmt.Sprintf(`[[daemon]]
name = "work"
local = true
home = %q
instance_uid = %q
`, workHome, uid)), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.toml"), []byte("version = 1\n[project]\nname = \"spoke-project\"\n"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.local.toml"), []byte("version = 1\n[server]\ndaemon = \"work\"\n"), 0600))

			var mutationAuthorization string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/ping" {
					_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
					return
				}
				if strings.HasSuffix(r.URL.Path, "/actions/"+tc.verb) {
					mutationAuthorization = r.Header.Get("Authorization")
				}
				if r.Header.Get("Authorization") != "Bearer work-token" {
					http.Error(w, "wrong profile credential", http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/api/v1/instance":
					_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
				case "/api/v1/projects/resolve":
					_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"}}`)
				case "/api/v1/projects/1/issues/abc4":
					_, _ = fmt.Fprint(w, `{"issue":{"short_id":"abc4"}}`)
				case "/api/v1/projects/1/issues/abc4/actions/" + tc.verb:
					_, _ = fmt.Fprint(w, tc.response)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: workHome, InstanceUID: uid})
			require.NoError(t, err)
			ns, err := daemon.NewNamespaceForHome(workHome, profile.StorageID)
			require.NoError(t, err)
			require.NoError(t, ns.EnsureDirs())
			_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"})
			require.NoError(t, err)

			cmd := newRootCmd()
			cmd.SetArgs([]string{"--workspace", root, "--project", "spoke-project", tc.verb, "abc4", "--force", "--confirm", tc.confirm})
			err = cmd.Execute()

			require.NoError(t, err)
			assert.Equal(t, "Bearer work-token", mutationAuthorization, "destructive request must keep the selected profile credential")
			assert.NotEqual(t, "Bearer personal-token", mutationAuthorization)
		})
	}
}

func TestLocalProfileTargetErrorsAreStructured(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  string
	}{
		{client.ErrProfileStorageUnavailable, "missing_profile_storage"},
		{client.ErrProfileIdentityMismatch, "wrong_database"},
		{client.ErrProfileSchemaMismatch, "version_mismatch"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			err := cliDaemonTargetError(tc.cause)
			var target *cliError
			require.ErrorAs(t, err, &target)
			assert.Equal(t, tc.code, target.Code)
			assert.Equal(t, ExitDaemonUnavail, target.ExitCode)
			assert.Contains(t, target.Message, "daemon diagnose")
		})
	}
}
