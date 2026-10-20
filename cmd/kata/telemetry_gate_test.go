package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/hooks"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestContractHookQuietPreflight(t *testing.T) {
	for _, selection := range []string{"remote", "named remote", "local profile", "default local", "injected fallback"} {
		t.Run(selection, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
			t.Setenv("KATA_DSN", "")
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_AUTH_TOKEN", "")
			t.Setenv(hooks.HookVersionEnv, "1")
			t.Setenv("KATA_AGENT_USE_TEST_DISCOVER", "1")
			const uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"
			var posts, probes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/ping":
					probes.Add(1)
					_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"test"}`)
				case "/api/v1/instance":
					probes.Add(1)
					_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
				case "/api/v1/ui/telemetry":
					posts.Add(1)
					w.WriteHeader(http.StatusAccepted)
					_, _ = fmt.Fprint(w, `{"status":"disabled"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			switch selection {
			case "remote":
				t.Setenv("KATA_SERVER", server.URL)
			case "named remote":
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname = \"work\"\nurl = %q\n", server.URL)), 0600))
				t.Setenv("KATA_AGENT_USE_TEST_DAEMON", "work")
			case "local profile", "default local":
				var ns *daemon.Namespace
				var err error
				if selection == "local profile" {
					profileHome := t.TempDir()
					require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("active_daemon = \"work\"\n[[daemon]]\nname = \"work\"\nlocal = true\nhome = %q\ninstance_uid = %q\n", profileHome, uid)), 0600))
					profile, resolveErr := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: profileHome, InstanceUID: uid})
					require.NoError(t, resolveErr)
					ns, err = daemon.NewNamespaceForHome(profileHome, profile.StorageID)
				} else {
					ns, err = daemon.NewNamespace()
				}
				require.NoError(t, err)
				require.NoError(t, ns.EnsureDirs())
				_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"})
				require.NoError(t, err)
			case "injected fallback":
				t.Setenv("KATA_AGENT_USE_TEST_INJECT", "1")
			}
			report := func() {
				process := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAgentUseFullDayProcess$") //nolint:gosec // Current test executable with fixed fixture arguments.
				process.Env = append(os.Environ(), "KATA_AGENT_USE_TEST_SERVER="+server.URL)
				output, err := process.CombinedOutput()
				require.NoError(t, err, "%s", output)
			}
			report()
			require.EqualValues(t, 1, posts.Load(), "discovery still reports when preflight has no receipt or no key")
			initialProbes := probes.Load()
			if selection == "named remote" || selection == "local profile" || selection == "default local" {
				require.Positive(t, initialProbes)
			}
			report()
			require.EqualValues(t, 1, posts.Load(), "a fresh hook reuses the disabled receipt")
			require.Equal(t, initialProbes, probes.Load(), "a disabled receipt suppresses discovery probes")
		})
	}
}
