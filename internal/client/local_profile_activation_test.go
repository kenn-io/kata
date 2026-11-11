package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestSocketActivationProfileNeverSpawns(t *testing.T) {
	for _, state := range []string{"stopped", "stale"} {
		t.Run(state, func(t *testing.T) {
			profile, root := localProfileFixture(t)
			t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
			require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0600))

			var instanceRequests atomic.Int32
			if state == "stale" {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/v1/ping":
						_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":"old-build","pid":%d}`, os.Getpid())
					case "/api/v1/instance":
						instanceRequests.Add(1)
						_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, profile.InstanceUID)
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(server.Close)
				ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
				require.NoError(t, err)
				require.NoError(t, ns.EnsureDirs())
				_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
					PID:       os.Getpid(),
					Network:   "tcp",
					Address:   strings.TrimPrefix(server.URL, "http://"),
					StartedAt: time.Now(),
				})
				require.NoError(t, err)
			}

			oldVersion, oldStop, oldStart := currentVersionForEnsure, stopRunningDaemonsForEnsure, startDetachedDaemonForEnsure
			t.Cleanup(func() {
				currentVersionForEnsure, stopRunningDaemonsForEnsure, startDetachedDaemonForEnsure = oldVersion, oldStop, oldStart
			})
			currentVersionForEnsure = func() string { return "new-build" }
			stopCalls, startCalls := 0, 0
			stopRunningDaemonsForEnsure = func(context.Context, string, string) error {
				stopCalls++
				return nil
			}
			// Intercept the real profile startup seam even if the ownership guard
			// regresses: no mutant may launch a process or wait for readiness.
			startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
				startCalls++
				return errors.New("unexpected detached profile startup")
			}

			resolved, err := EnsureResolvedInWorkspace(t.Context(), root)

			assert.Zero(t, stopCalls, "manager-owned profile must not be signaled")
			assert.Zero(t, startCalls, "manager-owned profile must not be spawned")
			if state == "stale" {
				assert.Positive(t, instanceRequests.Load(), "stale profile must pass its live identity probe before refusal")
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), `socket_activation="systemd"`)
			assert.Contains(t, err.Error(), "use systemd")
			assert.Contains(t, err.Error(), "service")
			require.NotNil(t, resolved.LocalProfile)
			assert.Equal(t, profile.Home, resolved.LocalProfile.Home)
			assert.Equal(t, profile.InstanceUID, resolved.LocalProfile.InstanceUID)
		})
	}
}
