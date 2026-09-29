package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEmbeddingCredentialResolution(t *testing.T) {
	t.Setenv("EXAMPLE_EMBEDDING_KEY", "env-secret")
	t.Setenv("EXAMPLE_UNSET_KEY", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(home)
	path := filepath.Join(home, "embedding.key")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, inline, file, env, key, source, reason string }{
		{name: "inline precedence", inline: "inline-secret", file: path, env: "EXAMPLE_EMBEDDING_KEY", key: "inline-secret", source: "inline"},
		{name: "file precedence", file: path, env: "EXAMPLE_EMBEDDING_KEY", key: "file-secret", source: "file:" + path},
		{name: "tilde", file: "~/embedding.key", key: "file-secret", source: "file:~/embedding.key"},
		{name: "env", env: "EXAMPLE_EMBEDDING_KEY", key: "env-secret", source: "env:EXAMPLE_EMBEDDING_KEY"},
		{name: "missing env", env: "EXAMPLE_UNSET_KEY", source: "env:EXAMPLE_UNSET_KEY", reason: "unset"},
		{name: "missing file does not fall through", file: path + ".missing", env: "EXAMPLE_EMBEDDING_KEY", source: "file:" + path + ".missing", reason: "cannot read"},
		{name: "relative file does not fall through", file: "embedding.key", env: "EXAMPLE_EMBEDDING_KEY", source: "file:embedding.key", reason: "absolute path"},
		{name: "no source allows keyless providers", source: "none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := (EmbeddingsConfig{APIKey: tt.inline, APIKeyFile: tt.file, APIKeyEnv: tt.env}).ResolveCredential()
			if c.Key != tt.key || c.Source != tt.source || !strings.Contains(c.Reason, tt.reason) || (tt.reason == "" && c.Reason != "") {
				t.Fatalf("unexpected credential: source=%q reason=%q key matches=%t", c.Source, c.Reason, c.Key == tt.key)
			}
			if strings.Contains(c.Reason, "secret") {
				t.Fatal("reason contains secret")
			}
		})
	}
}

func TestEmbeddingCredentialFileSafety(t *testing.T) {
	t.Setenv("EXAMPLE_EMBEDDING_KEY", "env-secret")
	path := filepath.Join(t.TempDir(), "embedding.key")
	for _, tt := range []struct {
		name, contents, reason string
		mode                   os.FileMode
	}{
		{"empty", "\n", "empty", 0600},
		{"world readable", "file-secret", "group/world-readable", 0644},
		{"oversized", strings.Repeat("x", (64<<10)+1), "exceeds 64 KiB", 0600},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tt.mode == 0644 {
				t.Skip("Unix permissions")
			}
			if err := os.WriteFile(path, []byte(tt.contents), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			c := (EmbeddingsConfig{APIKeyFile: path, APIKeyEnv: "EXAMPLE_EMBEDDING_KEY"}).ResolveCredential() //nolint:gosec // G101: synthetic environment variable name, not a credential.
			if c.Key != "" || c.Source != "file:"+path || !strings.Contains(c.Reason, tt.reason) {
				t.Fatalf("unexpected reason %q", c.Reason)
			}
		})
	}
}

func TestReadDaemonConfigEmbeddingKeyFilePrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	path := filepath.Join(home, "embedding.key")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[search.embeddings]\nbase_url = \"https://embedding.example/v1\"\nmodel = \"example-model\"\napi_key = \"inline-secret\"\napi_key_file = %q\napi_key_env = \"EXAMPLE_EMBEDDING_KEY\"\n", path)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadDaemonConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Search.Embeddings.ResolveCredential().Key != "inline-secret" {
		t.Fatal("inline must win")
	}
}
