package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/docbank"
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
	writePrivateCredentialFixture(t, path, key+"\n")
	return path
}

// stubLocalDocbank points setup at an empty Docbank home and replaces how it starts Docbank.
func stubLocalDocbank(t *testing.T, start func(ctx context.Context, home string) error) string {
	t.Helper()
	docbankHome := t.TempDir()
	t.Setenv("DOCBANK_HOME", docbankHome)
	original := localDocbank
	t.Cleanup(func() { localDocbank = original })
	localDocbank = docbank.Local{Start: start}
	return docbankHome
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
	require.Equal(t, original+"\n[docbank]\nurl = \""+fake.Server.URL+"\"\napi_key_file = "+strconv.Quote(keyFile)+"\nfolder = \"/kata\"\n", string(written))
	require.Contains(t, stderr, "can read any file in the Docbank vault at "+fake.Server.URL)
	require.Contains(t, out, fake.Server.URL)
}

func TestDocbankSetupRefusesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name, key, config, env, wantErr string
		wantRequest                     bool
	}{
		{name: "rejected key", key: "wrong-key", wantRequest: true, wantErr: "rejected the API key (HTTP 401)"},
		{name: "empty section", key: "test-key", config: "[docbank]\n", wantRequest: true, wantErr: "already sets docbank keys"},
		{name: "different section", key: "test-key", config: "[docbank]\nurl = \"https://docbank.example\"\napi_key_env = \"EXAMPLE_KEY\"\n", wantErr: "already has a different [docbank] section"},
		{name: "environment override", key: "test-key", env: "KATA_DOCBANK_FOLDER", wantErr: "unset KATA_DOCBANK_FOLDER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupDocbankEnv(t)
			fake := docbanktest.New(t)
			if tc.env != "" {
				t.Setenv(tc.env, "/elsewhere")
			}
			path := filepath.Join(home, "config.toml")
			original := "# operator notes stay\n" + tc.config
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

			_, _, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--url", fake.Server.URL, "--key-file", writeDocbankKey(t, tc.key), "--yes")

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

	require.JSONEq(t, `{"kata_api_version":1,"local":false,"url":"`+fake.Server.URL+`","folder":"/kata","key_file":`+strconv.Quote(keyFile)+`,"changed":false}`, out)
	second, err := os.ReadFile(path) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
}

func TestDocbankSetupLocal(t *testing.T) {
	setupDocbankEnv(t)
	home := filepath.Join(t.TempDir(), "not-yet-created")
	t.Setenv("KATA_HOME", home)
	docbankHome := stubLocalDocbank(t, func(context.Context, string) error { return errors.New("must not start a running Docbank") })
	fake := docbanktest.New(t)
	fake.Publish(t, docbankHome)

	out, stderr, err := executeRootCapture(t, t.Context(), "--agent", "docbank", "setup", "--yes")
	require.NoError(t, err, stderr)

	require.Contains(t, fake.Requests(), "GET /api/v1/path")
	written, err := os.ReadFile(filepath.Join(home, "config.toml")) // #nosec G304 -- test-owned configuration
	require.NoError(t, err)
	require.Equal(t, "[docbank]\nlocal = true\nfolder = \"/kata\"\n", string(written))
	require.Contains(t, stderr, "can read any file in the Docbank vault at "+docbankHome)
	require.Equal(t, "OK docbank-setup local=true folder=/kata changed=true\n", out)
}

func TestDocbankSetupLocalRefusesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "docbank cannot start", args: []string{"--yes"}, wantErr: "stub cannot start docbank"},
		{name: "key file without url", args: []string{"--key-file", "/elsewhere", "--yes"}, wantErr: "--key-file goes with --url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupDocbankEnv(t)
			stubLocalDocbank(t, func(context.Context, string) error { return errors.New("stub cannot start docbank") })
			path := filepath.Join(home, "config.toml")
			original := "# operator notes stay\n"
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

			_, _, err := executeRootCapture(t, t.Context(), append([]string{"docbank", "setup"}, tc.args...)...)

			require.ErrorContains(t, err, tc.wantErr)
			written, readErr := os.ReadFile(path) // #nosec G304 -- test-owned configuration
			require.NoError(t, readErr)
			require.Equal(t, original, string(written))
		})
	}
}

func TestDocbankSetupWithoutApprovalStartsNothing(t *testing.T) {
	home := setupDocbankEnv(t)
	stubIsTTY(t, false)
	started := false
	stubLocalDocbank(t, func(context.Context, string) error { started = true; return nil })
	path := filepath.Join(home, "config.toml")
	original := "# operator notes stay\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	_, _, err := executeRootCapture(t, t.Context(), "docbank", "setup")

	require.ErrorContains(t, err, "no TTY")
	require.False(t, started, "Docbank must not start before the change is approved")
	written, readErr := os.ReadFile(path) // #nosec G304 -- test-owned configuration
	require.NoError(t, readErr)
	require.Equal(t, original, string(written))
}

func TestDocbankSetupReportsMissingKeyFileBeforeAsking(t *testing.T) {
	setupDocbankEnv(t)
	stubIsTTY(t, false)

	_, stderr, err := executeRootCapture(t, t.Context(), "docbank", "setup", "--url", "https://docbank.example", "--key-file", filepath.Join(t.TempDir(), "missing.key"))

	require.ErrorContains(t, err, "missing.key")
	require.NotContains(t, stderr, "can read any file", "a key file that can't be read fails before the warning and question")
}
