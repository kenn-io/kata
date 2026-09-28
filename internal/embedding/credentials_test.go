package embedding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.kenn.io/kata/internal/config"
)

func TestMissingCredentialDoesNotCallProvider(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredential(config.EmbeddingCredential{Source: "env:EXAMPLE_KEY", Reason: "no embedding API key (env EXAMPLE_KEY is unset)"})
	_, err = c.Embed(context.Background(), []string{"query"})
	if err == nil || !strings.Contains(err.Error(), "no embedding API key") || calls.Load() != 0 {
		t.Fatalf("error=%v provider requests=%d", err, calls.Load())
	}
	h := c.CredentialHealth()
	if h.Credential != "missing" || h.CredentialSource != "env:EXAMPLE_KEY" || h.LastSuccessAt != nil {
		t.Fatalf("unexpected health %+v", h)
	}
}

func TestCredentialRejectionAndRecovery(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer example-secret" {
					t.Error("wrong credential")
				}
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte("provider echoed example-secret"))
					return
				}
				_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
			}))
			defer srv.Close()
			c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2, APIKey: "example-secret"})
			c.SetCredential(config.EmbeddingCredential{Key: "example-secret", Source: "file:example.key"})
			_, err := c.Embed(context.Background(), []string{"query"})
			var ce *CredentialError
			var ae *APIError
			if !errors.As(err, &ce) || !errors.As(err, &ae) || ae.StatusCode != status || strings.Contains(err.Error(), "example-secret") || !strings.Contains(err.Error(), "rejected the API key") {
				t.Fatalf("unexpected error %v", err)
			}
			h := c.CredentialHealth()
			if h.Credential != "rejected" || h.LastErrorAt == nil || h.LastError == "" || h.LastSuccessAt != nil {
				t.Fatalf("unexpected rejected health %+v", h)
			}
			c.SetCredential(config.EmbeddingCredential{Key: "example-secret", Source: "file:example.key"})
			if c.CredentialHealth().Credential != "rejected" {
				t.Fatal("unchanged reload cleared rejection")
			}
			if _, err := c.Embed(context.Background(), []string{"query"}); err != nil {
				t.Fatal(err)
			}
			h = c.CredentialHealth()
			if h.Credential != "ok" || h.LastError != "" || h.LastErrorAt != nil || h.LastSuccessAt == nil {
				t.Fatalf("unexpected recovered health %+v", h)
			}
		})
	}
}

func TestCredentialReloadFencesOldResponse(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(entered)
				<-release
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
			}))
			defer srv.Close()
			c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2, APIKey: "old-secret"})
			done := make(chan error, 1)
			go func() { _, err := c.Embed(context.Background(), []string{"query"}); done <- err }()
			<-entered
			c.SetCredential(config.EmbeddingCredential{Source: "env:EXAMPLE_KEY", Reason: "no embedding API key"})
			close(release)
			<-done
			h := c.CredentialHealth()
			if h.Credential != "missing" || h.LastErrorAt != nil || h.LastSuccessAt != nil {
				t.Fatalf("old response overwrote reload %+v", h)
			}
		})
	}
}

func TestEmptyEmbedDoesNotReportSuccess(t *testing.T) {
	c, _ := New(Config{BaseURL: "https://embedding.example", Model: "m", APIKey: "example-key"})
	if _, err := c.Embed(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if c.CredentialHealth().LastSuccessAt != nil {
		t.Fatal("empty call reported provider success")
	}
}
