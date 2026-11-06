package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

func setupDocbankEnv(t *testing.T) string {
	t.Helper()
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	for _, key := range []string{"KATA_DOCBANK_URL", "KATA_DOCBANK_API_KEY_FILE", "KATA_DOCBANK_FOLDER"} {
		t.Setenv(key, "")
	}
	return home
}

func writeDocbankKey(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docbank.key")
	require.NoError(t, os.WriteFile(path, []byte(key+"\n"), 0o600))
	return path
}

func TestDocbankSetupRemote(t *testing.T) {
	home := setupDocbankEnv(t)
	fake := docbanktest.New(t)
	keyFile := writeDocbankKey(t, "test-key")
	path := filepath.Join(home, "config.toml")
	original := "# operator notes stay\nlisten = \"127.0.0.1:7777\"\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	out, stderr, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--url", fake.Server.URL, "--key-file", keyFile, "--yes")
	require.NoError(t, err, stderr)

	require.Contains(t, fake.Requests(), "GET /api/v1/path")
	written, err := os.ReadFile(path) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(written), original), "setup must append, keeping the existing text:\n%s", written)
	got, err := config.ParseDocbankFile(written)
	require.NoError(t, err)
	require.Equal(t, config.DocbankConfig{URL: fake.Server.URL, APIKeyFile: keyFile, Folder: "/kata"}, got)
	require.Contains(t, stderr, "can read any file in this Docbank")
	require.Contains(t, out, fake.Server.URL)
}

func TestDocbankSetupRefusesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name, key, url, config, env, wantErr string
		wantRequest                          bool
	}{
		{name: "rejected key", key: "wrong-key", wantRequest: true, wantErr: "rejected the API key (HTTP 401)"},
		{name: "plain HTTP to another host", key: "test-key", url: "http://docbank.example", wantErr: "requires HTTPS except on loopback"},
		{name: "different section", key: "test-key", config: "[docbank]\nurl = \"https://docbank.example\"\napi_key_env = \"EXAMPLE_KEY\"\n", wantErr: "already has a different [docbank] section"},
		{name: "environment override", key: "test-key", env: "KATA_DOCBANK_FOLDER", wantErr: "unset KATA_DOCBANK_FOLDER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupDocbankEnv(t)
			fake := docbanktest.New(t)
			if tc.env != "" {
				t.Setenv(tc.env, "/elsewhere")
			}
			url := tc.url
			if url == "" {
				url = fake.Server.URL
			}
			path := filepath.Join(home, "config.toml")
			original := "# operator notes stay\n" + tc.config
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

			_, _, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--url", url, "--key-file", writeDocbankKey(t, tc.key), "--yes")

			require.ErrorContains(t, err, tc.wantErr)
			written, readErr := os.ReadFile(path) // #nosec G304 -- test-owned configuration
			require.NoError(t, readErr)
			require.Equal(t, original, string(written))
			require.Equal(t, tc.wantRequest, len(fake.Requests()) > 0)
		})
	}
}

func TestDocbankSetupRerunChangesNothing(t *testing.T) {
	home := setupDocbankEnv(t)
	fake := docbanktest.New(t)
	keyFile := writeDocbankKey(t, "test-key")
	_, stderr, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--url", fake.Server.URL, "--key-file", keyFile, "--yes")
	require.NoError(t, err, stderr)
	path := filepath.Join(home, "config.toml")
	first, err := os.ReadFile(path) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)

	out, stderr, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--url", fake.Server.URL, "--key-file", keyFile, "--json")
	require.NoError(t, err, stderr)

	require.JSONEq(t, `{"kata_api_version":1,"url":"`+fake.Server.URL+`","folder":"/kata","key_file":`+strconv.Quote(keyFile)+`,"changed":false}`, out)
	second, err := os.ReadFile(path) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
}

// fakeDocbankCLI answers the Docbank commands setup runs. Its restart reads [server] from the
// vault's config.toml, as Docbank does, and serves the fake on that port, accepting only that key.
type fakeDocbankCLI struct {
	t     *testing.T
	vault string
	fake  *docbanktest.Fake
	calls []string
	addr  string
	// portOverride makes restart ignore api_port, as DOCBANK_API_PORT in Docbank's environment does.
	portOverride bool
}

func stubDocbankCLI(t *testing.T) *fakeDocbankCLI {
	t.Helper()
	f := &fakeDocbankCLI{t: t, vault: filepath.Join(t.TempDir(), "vault"), fake: docbanktest.New(t)}
	original := runDocbank
	t.Cleanup(func() { runDocbank = original })
	runDocbank = f.run
	return f
}

func (f *fakeDocbankCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	f.calls = append(f.calls, command)
	switch command {
	case "vault info --json":
		return json.Marshal(map[string]any{"vault_id": "00000000-0000-4000-8000-000000000001", "vault_path": f.vault, "live_files": 0})
	case "daemon status":
		if f.addr == "" {
			return []byte("daemon not running\n"), errors.New("exit status 1")
		}
		return []byte("running: pid 4242 at " + f.addr + " (0.17.0), up 1s\n"), nil
	case "daemon restart":
		var file struct {
			Server struct {
				APIPort int    `toml:"api_port"`
				APIKey  string `toml:"api_key"`
			} `toml:"server"`
		}
		_, err := toml.DecodeFile(filepath.Join(f.vault, "config.toml"), &file)
		require.NoError(f.t, err)
		port := strconv.Itoa(file.Server.APIPort)
		if f.portOverride {
			port = "0" // DOCBANK_API_PORT in Docbank's environment wins over its config
		}
		listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:"+port)
		require.NoError(f.t, err)
		target, err := url.Parse(f.fake.Server.URL)
		require.NoError(f.t, err)
		forward := httputil.NewSingleHostReverseProxy(target)
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Api-Key") != file.Server.APIKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r.Header.Set("X-Api-Key", "test-key")
			forward.ServeHTTP(w, r)
		})}
		go func() { _ = server.Serve(listener) }()
		f.t.Cleanup(func() { _ = server.Close() })
		f.addr = listener.Addr().String()
		return []byte("restarted\n"), nil
	}
	f.t.Fatalf("unexpected docbank command %q", command)
	return nil, nil
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

func TestDocbankSetupLocalFresh(t *testing.T) {
	home := filepath.Join(setupDocbankEnv(t), "not-yet-created")
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
	cli := stubDocbankCLI(t)
	require.NoError(t, os.MkdirAll(cli.vault, 0o700))
	docbankConfig := filepath.Join(cli.vault, "config.toml")
	docbankOriginal := "# docbank notes stay\n[watch]\nenabled = false\n"
	require.NoError(t, os.WriteFile(docbankConfig, []byte(docbankOriginal), 0o600))
	port := freeLoopbackPort(t)

	out, stderr, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--port", strconv.Itoa(port), "--yes")
	require.NoError(t, err, stderr)

	written, err := os.ReadFile(docbankConfig) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(written), docbankOriginal), "setup must append to Docbank's config:\n%s", written)
	var docbankFile struct {
		Server struct {
			BindAddr string `toml:"bind_addr"`
			APIPort  int    `toml:"api_port"`
			APIKey   string `toml:"api_key"`
		} `toml:"server"`
	}
	_, err = toml.Decode(string(written), &docbankFile)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", docbankFile.Server.BindAddr)
	require.Equal(t, port, docbankFile.Server.APIPort)
	require.Regexp(t, `^[0-9a-f]{64}$`, docbankFile.Server.APIKey)

	kataConfig, err := os.ReadFile(filepath.Join(home, "config.toml")) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)
	section, err := config.ParseDocbankFile(kataConfig)
	require.NoError(t, err)
	keyFile := filepath.Join(home, "docbank.key")
	require.Equal(t, config.DocbankConfig{URL: "http://127.0.0.1:" + strconv.Itoa(port), APIKeyFile: keyFile, Folder: "/kata"}, section)
	key, err := section.ResolveKey()
	require.NoError(t, err, "the daemon must accept the key file setup wrote")
	require.Equal(t, docbankFile.Server.APIKey, key)
	require.Contains(t, cli.calls, "daemon restart")
	require.NotContains(t, out+stderr, key)
}

func TestDocbankSetupLocalReusesRunningServer(t *testing.T) {
	home := setupDocbankEnv(t)
	cli := stubDocbankCLI(t)
	require.NoError(t, os.MkdirAll(cli.vault, 0o700))
	port := freeLoopbackPort(t)
	docbankConfig := filepath.Join(cli.vault, "config.toml")
	docbankOriginal := "[server]\nbind_addr = \"127.0.0.1\"\napi_port = " + strconv.Itoa(port) + "\napi_key = \"existing-key\"\n"
	require.NoError(t, os.WriteFile(docbankConfig, []byte(docbankOriginal), 0o600))
	_, err := cli.run(t.Context(), "daemon", "restart")
	require.NoError(t, err)
	cli.calls = nil

	_, stderr, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--yes")
	require.NoError(t, err, stderr)

	written, err := os.ReadFile(docbankConfig) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)
	require.Equal(t, docbankOriginal, string(written))
	require.NotContains(t, cli.calls, "daemon restart", "a Docbank already serving its configured port keeps running")
	kataConfig, err := os.ReadFile(filepath.Join(home, "config.toml")) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)
	section, err := config.ParseDocbankFile(kataConfig)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:"+strconv.Itoa(port), section.URL)
	key, err := section.ResolveKey()
	require.NoError(t, err)
	require.Equal(t, "existing-key", key)
}

func TestDocbankSetupLocalRequiresDocbankOnItsPort(t *testing.T) {
	home := setupDocbankEnv(t)
	cli := stubDocbankCLI(t)
	cli.portOverride = true

	_, _, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--port", strconv.Itoa(freeLoopbackPort(t)), "--yes")

	require.ErrorContains(t, err, "did not come up at")
	_, statErr := os.Stat(filepath.Join(home, "config.toml"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "kata must not point at a Docbank that is not on that port")
}

func TestDocbankSetupLocalRefusesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name, docbank, kata, storedKey, wantErr string
		flagPort, busy                          bool
	}{
		{name: "server without key", docbank: "[server]\napi_port = PORT\n", wantErr: "without both api_port and api_key"},
		{name: "port differs from server", docbank: "[server]\napi_port = 1\napi_key = \"example\"\n", flagPort: true, wantErr: "differs from api_port 1"},
		{name: "stored key differs from server", docbank: "[server]\napi_port = PORT\napi_key = \"example\"\n", storedKey: "other", wantErr: "holds a different key"},
		{name: "port in use", flagPort: true, busy: true, wantErr: "in use by something other than this Docbank"},
		{name: "different kata section", flagPort: true, kata: "[docbank]\nurl = \"https://docbank.example\"\napi_key_env = \"EXAMPLE_KEY\"\n", wantErr: "already has a different [docbank] section"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupDocbankEnv(t)
			cli := stubDocbankCLI(t)
			require.NoError(t, os.MkdirAll(cli.vault, 0o700))
			port := freeLoopbackPort(t)
			if tc.busy {
				listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
			}
			docbankConfig := filepath.Join(cli.vault, "config.toml")
			docbankOriginal := "# docbank notes\n" + strings.ReplaceAll(tc.docbank, "PORT", strconv.Itoa(port))
			require.NoError(t, os.WriteFile(docbankConfig, []byte(docbankOriginal), 0o600))
			kataConfig := filepath.Join(home, "config.toml")
			kataOriginal := "# kata notes\n" + tc.kata
			require.NoError(t, os.WriteFile(kataConfig, []byte(kataOriginal), 0o600))
			keyFile := filepath.Join(home, "docbank.key")
			if tc.storedKey != "" {
				require.NoError(t, os.WriteFile(keyFile, []byte(tc.storedKey), 0o600))
			}
			args := []string{"docbank", "setup", "--yes"}
			if tc.flagPort {
				args = append(args, "--port", strconv.Itoa(port))
			}

			_, _, err := executeRootCapture(t, t.Context(), args...)

			require.ErrorContains(t, err, tc.wantErr)
			for file, want := range map[string]string{docbankConfig: docbankOriginal, kataConfig: kataOriginal} {
				got, readErr := os.ReadFile(file) // #nosec G304 -- test-owned configuration
				require.NoError(t, readErr)
				require.Equal(t, want, string(got))
			}
			stored, _ := os.ReadFile(keyFile) // #nosec G304 -- test-owned key file
			require.Equal(t, tc.storedKey, string(stored))
			require.NotContains(t, cli.calls, "daemon restart")
		})
	}
}
