package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestLocalProfileStorageIdentityReadOnly(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "kata.db")
	store, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	uid := store.InstanceUID()
	require.NoError(t, store.Close())
	entry := config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid}
	profile, err := config.ResolveLocalProfile(entry)
	require.NoError(t, err)
	identity, err := InspectLocalProfileStorage(ctx, profile)
	require.NoError(t, err)
	assert.Equal(t, uid, identity.InstanceUID)
	assert.Equal(t, db.CurrentSchemaVersion(), identity.SchemaVersion)
	profile.InstanceUID = "01HZZZZZZZZZZZZZZZZZZZZZ01"
	identity, err = InspectLocalProfileStorage(ctx, profile)
	require.Error(t, err)
	assert.Equal(t, uid, identity.InstanceUID)
	// The inspection handle cannot write, bootstrap, or replace this identity.
	store, err = sqlitestore.Open(ctx, path, db.ReadOnly())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	require.NoError(t, store.RefreshInstanceUID(ctx))
	assert.Equal(t, uid, store.InstanceUID())
}

func TestLocalProfileStorageMissingStaysMissing(t *testing.T) {
	home := t.TempDir()
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ01"})
	require.NoError(t, err)
	_, err = InspectLocalProfileStorage(context.Background(), profile)
	require.Error(t, err)
	_, err = os.Stat(profile.DSN)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalProfileUnreadableMetadataIsStorageUnavailable(t *testing.T) {
	for _, test := range []struct {
		name    string
		cause   string
		corrupt func(*sqlitestore.Store) error
	}{
		{
			name:  "instance identity",
			cause: "no such table: meta",
			corrupt: func(store *sqlitestore.Store) error {
				_, err := store.ExecContext(context.Background(), "DROP TABLE meta")
				return err
			},
		},
		{
			name:  "schema version",
			cause: "invalid syntax",
			corrupt: func(store *sqlitestore.Store) error {
				_, err := store.ExecContext(context.Background(), "UPDATE meta SET value='invalid' WHERE key='schema_version'")
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			store, err := sqlitestore.Open(ctx, filepath.Join(home, "kata.db"))
			require.NoError(t, err)
			instanceUID := store.InstanceUID()
			require.NoError(t, test.corrupt(store))
			require.NoError(t, store.Close())
			profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: instanceUID})
			require.NoError(t, err)

			_, err = InspectLocalProfileStorage(ctx, profile)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrProfileStorageUnavailable)
			assert.Contains(t, err.Error(), test.cause)
		})
	}
}

func TestLocalProfileStorageInspectionRetainsUnsupportedSchema(t *testing.T) {
	for _, version := range []int{db.CurrentSchemaVersion() - 1, db.CurrentSchemaVersion() + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			path := filepath.Join(home, "kata.db")
			store, err := sqlitestore.Open(ctx, path)
			require.NoError(t, err)
			uid := store.InstanceUID()
			_, err = store.ExecContext(ctx, "UPDATE meta SET value=? WHERE key='schema_version'", fmt.Sprint(version))
			require.NoError(t, err)
			require.NoError(t, store.Close())
			profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid})
			require.NoError(t, err)
			identity, err := InspectLocalProfileStorage(ctx, profile)
			require.NoError(t, err)
			assert.Equal(t, version, identity.SchemaVersion)
			inspected, err := sqlitestore.PeekSchemaVersion(ctx, path)
			require.NoError(t, err)
			assert.Equal(t, version, inspected)
		})
	}
}

func localProfileFixture(t *testing.T) (config.LocalProfileConfig, string) {
	t.Helper()
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	home := t.TempDir()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	uid := store.InstanceUID()
	require.NoError(t, store.Close())
	personalHome := os.Getenv("KATA_HOME")
	//nolint:gosec // The fixture created and assigned this temporary home.
	require.NoError(t, os.WriteFile(filepath.Join(personalHome, "config.toml"), []byte(fmt.Sprintf(`active_daemon = "work"
[[daemon]]
name = "work"
local = true
home = %q
instance_uid = %q
`, home, uid)), 0600))
	root := t.TempDir()
	writeNeutralWorkspaceMarker(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".kata.local.toml"), []byte("version = 1\n[server]\ndaemon = \"work\"\n"), 0600))
	profile, err := config.ResolveLocalProfile(config.CatalogDaemonConfig{Name: "work", Local: true, Home: home, InstanceUID: uid})
	require.NoError(t, err)
	return profile, root
}

func TestLocalProfileDiscoveryRetainsStoppedSelection(t *testing.T) {
	profile, root := localProfileFixture(t)
	personal := startResolvableDaemon(t)
	require.NoError(t, writeRuntimeRecord(t, os.Getenv("KATA_HOME"), strings.TrimPrefix(personal.URL, "http://")))
	selected, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	require.NotNil(t, selected.Profile)
	assert.Equal(t, profile.Home, selected.Profile.Home)
	assert.Equal(t, DaemonSourceLocalConfig, selected.Resolved.Source)
	resolved, ok, err := DiscoverResolvedInWorkspace(t.Context(), root)
	require.NoError(t, err)
	assert.False(t, ok)
	require.NotNil(t, resolved.LocalProfile)
	assert.Equal(t, profile.InstanceUID, resolved.LocalProfile.InstanceUID)
	assert.Empty(t, resolved.BaseURL)
	assert.False(t, resolved.ConfiguredRemote())
	_, err = os.Stat(filepath.Join(profile.Home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalProfileSelectionPrecedenceAndRefresh(t *testing.T) {
	profile, root := localProfileFixture(t)
	server := startResolvableDaemon(t)
	t.Setenv("KATA_SERVER", server.URL)
	selected, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	assert.Nil(t, selected.Profile)
	assert.Equal(t, DaemonSourceServerEnv, selected.Resolved.Source)
	selected, err = InspectSelection(t.Context(), root, "work")
	require.NoError(t, err)
	require.NotNil(t, selected.Profile)
	assert.Equal(t, profile.Home, selected.Profile.Home)
	assert.Equal(t, filepath.Join(os.Getenv("KATA_HOME"), "config.toml"), selected.Resolved.SourcePath)
	refreshed := selected.Resolved.WithRunning(localRunningDaemon(UnixBase, "unix:///tmp/work.sock"))
	require.NotNil(t, refreshed.LocalProfile)
	assert.Equal(t, profile.InstanceUID, refreshed.LocalProfile.InstanceUID)
	assert.False(t, refreshed.ConfiguredRemote())
}

func TestLocalProfileLiveIdentityUsesWorkCredential(t *testing.T) {
	profile, root := localProfileFixture(t)
	t.Setenv("KATA_AUTH_TOKEN", "personal-token")
	require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("[auth]\ntoken = \"work-token\"\n"), 0600))
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"test"}`)
			return
		}
		got = r.Header.Get("Authorization")
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, profile.InstanceUID)
	}))
	defer server.Close()
	ns, err := daemon.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"})
	require.NoError(t, err)
	resolved, ok, err := DiscoverResolvedInWorkspace(t.Context(), root)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "Bearer work-token", got)
	assert.Equal(t, "work-token", resolved.Token)
	assert.Equal(t, server.URL, resolved.BaseURL)
}

func TestLocalProfileWrongDatabaseCannotStart(t *testing.T) {
	profile, root := localProfileFixture(t)
	cfg := fmt.Sprintf(`[[daemon]]
name = "work"
local = true
home = %q
instance_uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"
`, profile.Home)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("KATA_HOME"), "config.toml"), []byte(cfg), 0600)) //nolint:gosec // KATA_HOME is the test fixture temporary home. //nolint:gosec // KATA_HOME is the test fixture temporary home.
	calls := 0
	previous := startDetachedDaemonForEnsure
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
		calls++
		return errors.New("unexpected startup")
	}
	defer func() { startDetachedDaemonForEnsure = previous }()
	_, err := EnsureResolvedInWorkspace(t.Context(), root)
	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestLocalProfileStartUsesSelectedEnvironment(t *testing.T) {
	profile, root := localProfileFixture(t)
	t.Setenv("KATA_AUTH_TOKEN", "personal-token")
	t.Setenv("PORT", "8888")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, profile.InstanceUID)
	}))
	defer server.Close()
	oldStart, oldDiscover := startDetachedDaemonForEnsure, discoverDaemonForAutoStart
	defer func() { startDetachedDaemonForEnsure = oldStart; discoverDaemonForAutoStart = oldDiscover }()
	var childEnv []string
	startDetachedDaemonForEnsure = func(_ context.Context, opts kitdaemon.StartDetachedOptions) error { childEnv = opts.Env; return nil }
	discoverDaemonForAutoStart = func(_ context.Context, dir string) (ensureDiscovery, error) {
		assert.Equal(t, filepath.Join(profile.Home, "runtime", profile.StorageID), dir)
		return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: liveDaemon{BaseURL: server.URL, Record: kitdaemon.RuntimeRecord{Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"}}}, nil
	}
	resolved, err := EnsureResolvedInWorkspace(t.Context(), root)
	require.NoError(t, err)
	assert.Equal(t, profile.Home, resolved.LocalProfile.Home)
	assert.Contains(t, childEnv, "KATA_HOME="+profile.Home)
	assert.NotContains(t, childEnv, "KATA_AUTH_TOKEN=personal-token")
	assert.NotContains(t, childEnv, "PORT=8888")
}

func TestLocalProfileUnsupportedSchemaCannotReplaceProcess(t *testing.T) {
	profile, root := localProfileFixture(t)
	store, err := sqlitestore.Open(t.Context(), profile.DSN)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "UPDATE meta SET value=? WHERE key='schema_version'", fmt.Sprint(db.CurrentSchemaVersion()+1))
	require.NoError(t, err)
	require.NoError(t, store.Close())
	calls := 0
	oldStop, oldStart := stopRunningDaemonsForEnsure, startDetachedDaemonForEnsure
	defer func() { stopRunningDaemonsForEnsure = oldStop; startDetachedDaemonForEnsure = oldStart }()
	stopRunningDaemonsForEnsure = func(context.Context, string, string) error { calls++; return nil }
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error { calls++; return nil }
	_, err = EnsureResolvedInWorkspace(t.Context(), root)
	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestLocalProfileNamedDiscoveryCannotUsePersonalRuntime(t *testing.T) {
	profile, _ := localProfileFixture(t)
	personal := startResolvableDaemon(t)
	require.NoError(t, writeRuntimeRecord(t, os.Getenv("KATA_HOME"), strings.TrimPrefix(personal.URL, "http://")))
	resolved, err := DiscoverResolvedNamed(t.Context(), "work")
	require.NoError(t, err)
	assert.Empty(t, resolved.BaseURL)
	_, err = os.Stat(filepath.Join(profile.Home, "runtime"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalProfileDiscoveryDoesNotRepairRuntimePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits")
	}
	profile, root := localProfileFixture(t)
	dir := filepath.Join(profile.Home, "runtime", profile.StorageID)
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.Chmod(dir, 0755)) //nolint:gosec // Regression fixture must retain deliberately non-private permissions.
	_, _, err := DiscoverResolvedInWorkspace(t.Context(), root)
	require.Error(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

func TestLocalProfileWorkspaceRequiresExplicitHome(t *testing.T) {
	for _, target := range []string{"local = true", `url = "https://daemon.example"`} {
		t.Run(target, func(t *testing.T) {
			_, root := localProfileFixture(t)
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("KATA_HOME"), "config.toml"), []byte("[[daemon]]\nname = \"work\"\n"+target+"\n"), 0600)) //nolint:gosec // KATA_HOME is the test fixture temporary home.
			_, err := InspectSelection(t.Context(), root, "")
			require.Error(t, err)
		})
	}
}

func TestLocalProfileHTTPClientIgnoresParentProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "parent proxy reached", http.StatusTeapot) }))
	defer proxy.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	transport.Proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original; transport.CloseIdleConnections() }()
	resolved := ResolvedDaemon{BaseURL: target.URL, Token: "work-token", LocalProfile: &LocalProfileIdentity{Home: t.TempDir()}}
	hc, err := NewHTTPClientForResolved(t.Context(), resolved, Opts{})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.URL+"/api/v1/instance", nil)
	require.NoError(t, err)
	response, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
}

func TestLocalProfileRemoteViewCannotFallThroughToActiveRemote(t *testing.T) {
	profile, root := localProfileFixture(t)
	cfg := fmt.Sprintf(`active_daemon = "shared"
[[daemon]]
name = "work"
local = true
home = %q
instance_uid = %q
[[daemon]]
name = "shared"
url = "http://127.0.0.1:1"
`, profile.Home, profile.InstanceUID)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("KATA_HOME"), "config.toml"), []byte(cfg), 0600)) //nolint:gosec // KATA_HOME is the test fixture temporary home. //nolint:gosec // KATA_HOME is the test fixture temporary home.
	base, remote, err := ResolveRemote(t.Context(), root)
	require.NoError(t, err)
	assert.False(t, remote)
	assert.Empty(t, base)
}

func TestLocalProfileEnsureSelectionKeepsSnapshot(t *testing.T) {
	profile, root := localProfileFixture(t)
	selection, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("KATA_HOME"), "config.toml"), []byte("[[daemon]]\nname = \"work\"\nlocal = true\n"), 0600)) //nolint:gosec // KATA_HOME is the test fixture temporary home.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, profile.InstanceUID)
	}))
	defer server.Close()
	oldStart, oldDiscover := startDetachedDaemonForEnsure, discoverDaemonForAutoStart
	defer func() { startDetachedDaemonForEnsure = oldStart; discoverDaemonForAutoStart = oldDiscover }()
	startDetachedDaemonForEnsure = func(_ context.Context, opts kitdaemon.StartDetachedOptions) error {
		assert.Contains(t, opts.Env, "KATA_HOME="+profile.Home)
		assert.Contains(t, opts.Env, "KATA_DSN="+profile.DSN)
		return nil
	}
	discoverDaemonForAutoStart = func(_ context.Context, _ string) (ensureDiscovery, error) {
		return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: liveDaemon{BaseURL: server.URL, Record: kitdaemon.RuntimeRecord{Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"}}}, nil
	}
	stopped, found, err := DiscoverLocalProfileSelection(t.Context(), selection)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, profile.Home, stopped.LocalProfile.Home)
	resolved, err := EnsureSameLocalProfile(t.Context(), selection.Resolved)
	require.NoError(t, err)
	assert.Equal(t, profile.Home, resolved.LocalProfile.Home)
	assert.Equal(t, profile.InstanceUID, resolved.LocalProfile.InstanceUID)
}

func TestLocalProfileWrongLiveUIDCannotReplace(t *testing.T) {
	profile, root := localProfileFixture(t)
	observed := "01HZZZZZZZZZZZZZZZZZZZZZ01"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, observed)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":"earlier-test-version","pid":%d}`, os.Getpid())
	}))
	defer server.Close()
	ns, err := daemon.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://")})
	require.NoError(t, err)
	calls := 0
	oldStop, oldStart := stopRunningDaemonsForEnsure, startDetachedDaemonForEnsure
	defer func() { stopRunningDaemonsForEnsure = oldStop; startDetachedDaemonForEnsure = oldStart }()
	stopRunningDaemonsForEnsure = func(context.Context, string, string) error { calls++; return nil }
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error { calls++; return nil }
	_, err = EnsureResolvedInWorkspace(t.Context(), root)
	require.ErrorIs(t, err, ErrProfileIdentityMismatch)
	var mismatch *LocalProfileIdentityError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, profile.InstanceUID, mismatch.Expected)
	assert.Equal(t, observed, mismatch.Observed)
	assert.Zero(t, calls)
}

func TestLocalProfileHTTPClientRejectsRedirectWithoutCredential(t *testing.T) {
	calls := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	defer other.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer target.Close()
	resolved := ResolvedDaemon{BaseURL: target.URL, LocalProfile: &LocalProfileIdentity{Home: t.TempDir()}}
	hc, err := NewHTTPClientForResolved(t.Context(), resolved, Opts{})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.URL+"/api/v1/instance", nil)
	require.NoError(t, err)
	response, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	assert.Zero(t, calls)
}

func TestLocalProfileEnsureSelectsAndVerifiesCompatibleRuntime(t *testing.T) {
	if os.Getppid() <= 0 {
		t.Skip("requires a live parent PID for a second runtime fixture")
	}
	for _, wrongUID := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongUID), func(t *testing.T) {
			profile, root := localProfileFixture(t)
			t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
			require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("[auth]\ntoken=\"work-token\"\n"), 0600))
			var probes atomic.Int32
			server := func(v string, pid int, uid string) *httptest.Server {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					probes.Add(1)
					if r.URL.Path == "/api/v1/ping" {
						_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, v, pid)
						return
					}
					assert.Equal(t, "Bearer work-token", r.Header.Get("Authorization"))
					_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
				}))
				t.Cleanup(s.Close)
				return s
			}
			stale := server("previous-binary", os.Getpid(), profile.InstanceUID)
			compatibleUID := profile.InstanceUID
			if wrongUID {
				compatibleUID = "01HZZZZZZZZZZZZZZZZZZZZZ01"
			}
			compatible := server(version.Version, os.Getppid(), compatibleUID)
			ns, err := daemon.NewNamespaceForHome(profile.Home, profile.StorageID)
			require.NoError(t, err)
			require.NoError(t, ns.EnsureDirs())
			for i, entry := range []struct {
				url string
				pid int
			}{{stale.URL, os.Getpid()}, {compatible.URL, os.Getppid()}} {
				_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: entry.pid, Network: "tcp", Address: strings.TrimPrefix(entry.url, "http://"), StartedAt: time.Now().Add(time.Duration(i) * time.Second)})
				require.NoError(t, err)
			}
			resolved, err := EnsureResolvedInWorkspace(t.Context(), root)
			if wrongUID {
				require.ErrorIs(t, err, ErrProfileIdentityMismatch)
			} else {
				require.NoError(t, err)
				assert.Equal(t, compatible.URL, resolved.BaseURL)
				assert.Equal(t, "work-token", resolved.Token)
				assert.Equal(t, int32(3), probes.Load(), "each runtime is probed once and the selected identity is checked once")
			}
		})
	}
}

func TestActivePlainLocalDaemonKeepsLocalDiscovery(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "global-token")
	require.NoError(t, writeRawConfig(home, `active_daemon = "local"
[[daemon]]
name = "local"
local = true
token = "stale-catalog-token"
`))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, version.Version, os.Getpid())
			return
		}
		if r.Header.Get("Authorization") != "Bearer global-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	require.NoError(t, writeRuntimeRecord(t, home, strings.TrimPrefix(server.URL, "http://")))
	resolved, err := PrepareResolvedInWorkspace(t.Context(), t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, server.URL, resolved.BaseURL)
	assert.False(t, resolved.ConfiguredRemote())
	assert.Equal(t, DaemonSourceLocalRuntime, resolved.Source)
	hc, err := NewHTTPClientForResolved(t.Context(), resolved, Opts{})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/protected", nil)
	require.NoError(t, err)
	response, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
}

func TestLocalProfileDiscoveryUsesLiveIdentityWithoutStorage(t *testing.T) {
	for _, wrongUID := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongUID), func(t *testing.T) {
			profile, root := localProfileFixture(t)
			require.NoError(t, os.Remove(profile.DSN))
			uid := profile.InstanceUID
			if wrongUID {
				uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/ping" {
					_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","pid":%d}`, os.Getpid())
					return
				}
				_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
			}))
			defer server.Close()
			ns, err := daemon.NewNamespaceForHome(profile.Home, profile.StorageID)
			require.NoError(t, err)
			require.NoError(t, ns.EnsureDirs())
			_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
				PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://"),
			})
			require.NoError(t, err)
			resolved, found, err := DiscoverResolvedInWorkspace(t.Context(), root)
			if wrongUID {
				require.ErrorIs(t, err, ErrProfileIdentityMismatch)
				assert.False(t, found)
			} else {
				require.NoError(t, err)
				assert.True(t, found)
				assert.Equal(t, server.URL, resolved.BaseURL)
			}
		})
	}
}
