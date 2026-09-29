package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.kenn.io/kata/internal/config"
)

func TestEmbeddingCredentialStartupAndReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	path := filepath.Join(home, "embedding.key")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer file-secret" {
			t.Error("wrong reloaded key")
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	ec := config.EmbeddingsConfig{BaseURL: srv.URL, Model: "m", Dims: 2, APIKeyFile: path}
	body := fmt.Sprintf("[search.embeddings]\nbase_url=%q\nmodel=\"m\"\ndims=2\napi_key_file=%q\n", srv.URL, path)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, _, err := preflightEmbeddingStartup(ec, filepath.Join(home, "kata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if h := c.CredentialHealth(); h.Credential != "missing" || h.CredentialSource != "file:"+path {
		t.Fatalf("unexpected startup health %+v", h)
	}
	if calls.Load() != 0 {
		t.Fatal("startup called provider")
	}
	if err := os.WriteFile(path, []byte("file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	woke := false
	if err := reloadEmbeddingCredentials(ec, c, func() { woke = true }); err != nil {
		t.Fatal(err)
	}
	if !woke {
		t.Fatal("reload did not wake backlog")
	}
	if _, err := c.Embed(context.Background(), []string{"query"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || c.CredentialHealth().Credential != "ok" {
		t.Fatal("credential reload did not restore provider calls")
	}
}

func TestEmbeddingReloadDoesNotMisrouteChangedProviderKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	ec := config.EmbeddingsConfig{BaseURL: "https://old.embedding.example/v1", Model: "m", APIKey: "old-secret"}
	c, _, err := preflightEmbeddingStartup(ec, filepath.Join(home, "kata.db"))
	if err != nil {
		t.Fatal(err)
	}
	body := "[search.embeddings]\nbase_url=\"https://new.embedding.example/v1\"\nmodel=\"m\"\napi_key=\"new-secret\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	woke := false
	err = reloadEmbeddingCredentials(ec, c, func() { woke = true })
	if err == nil || !strings.Contains(err.Error(), "restart") || woke {
		t.Fatalf("unexpected reload: %v woke=%t", err, woke)
	}
	if c.CredentialHealth().Credential != "ok" {
		t.Fatal("provider change mutated running credential")
	}
}
