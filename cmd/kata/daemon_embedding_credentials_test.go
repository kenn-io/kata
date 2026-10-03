package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
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
	writePrivateCredentialFixture(t, path, "file-secret\n")
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

func TestEmbeddingCredentialReloadRetriesRejectedBacklog(t *testing.T) {
	home := setupKataEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	defer func() { require.NoError(t, store.Close()) }()
	proj, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "pending", Author: "agent"})
	require.NoError(t, err)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer reloaded-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	ec := config.EmbeddingsConfig{BaseURL: srv.URL, Model: "m", Dims: 2, APIKey: "rejected-key"}
	client, _, err := preflightEmbeddingStartup(ec, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	workers := newDaemonWorkerGroup()
	cycles := make(chan struct{}, 1)
	admission := func() (*activity.Lease, bool, <-chan struct{}) {
		return activity.NewLease(func() { cycles <- struct{}{} }, nil), true, nil
	}
	_, idx, health, retry, err := startEmbeddingReconciler(ctx, workers, admission, ec, client, filepath.Join(home, "vectors.db"), store, daemon.NewEventBroadcaster(), log.New(io.Discard, "", 0))
	require.NoError(t, err)
	defer func() {
		cancel()
		require.True(t, workers.Wait(context.Background()))
		require.NoError(t, idx.Close())
	}()
	select {
	case <-cycles:
	case <-time.After(time.Second):
		t.Fatal("initial rejection did not complete")
	}
	require.Equal(t, http.StatusUnauthorized, health().LastErrorStatus)
	require.Equal(t, int64(1), health().Backlog)
	require.Equal(t, int32(1), calls.Load())

	body := fmt.Sprintf("[search.embeddings]\nbase_url=%q\nmodel=\"m\"\ndims=2\napi_key=\"reloaded-key\"\n", srv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600))
	require.NoError(t, reloadEmbeddingCredentials(ec, client, retry))
	select {
	case <-cycles:
	case <-time.After(time.Second):
		t.Fatal("credential reload did not retry pending work immediately")
	}
	require.Zero(t, health().Backlog)
	require.Equal(t, int64(1), health().Embedded)
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
