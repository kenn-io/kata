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

func TestKeylessProviderRequestsAndRecovery(t *testing.T) {
	for _, firstStatus := range []int{200, 401, 403} {
		t.Run(http.StatusText(firstStatus), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, present := r.Header["Authorization"]; present {
					t.Error("keyless request included Authorization")
				}
				if calls.Add(1) == 1 && firstStatus != 200 {
					w.WriteHeader(firstStatus)
					return
				}
				_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
			}))
			defer srv.Close()
			c, err := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2})
			if err != nil {
				t.Fatal(err)
			}
			vectors, err := c.Embed(context.Background(), []string{"query"})
			if calls.Load() != 1 {
				t.Fatalf("keyless provider received %d requests: %v", calls.Load(), err)
			}
			if firstStatus == 200 {
				if err != nil || len(vectors) != 1 || vectors[0][0] != 1 {
					t.Fatalf("keyless embedding: %v, %v", vectors, err)
				}
			} else {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != firstStatus {
					t.Fatalf("provider error: %v", err)
				}
				wantState, wantReason := "missing", "provider requires an API key"
				if firstStatus == 403 {
					wantState, wantReason = "rejected", "provider denied access (403)"
				}
				if h := c.CredentialHealth(); h.Credential != wantState || !strings.Contains(h.CredentialReason, wantReason) {
					t.Fatalf("provider health: %+v", h)
				}
				// An unchanged reload must not erase the provider's diagnosis or
				// prevent a later keyless request from recovering.
				c.SetCredential(config.EmbeddingCredential{Source: "none"})
				if c.CredentialHealth().Credential != wantState {
					t.Fatal("unchanged reload erased provider rejection")
				}
				if _, err := c.Embed(context.Background(), []string{"retry"}); err != nil {
					t.Fatal(err)
				}
			}
			if h := c.CredentialHealth(); h.Credential != "ok" || h.CredentialSource != "none" || h.LastError != "" {
				t.Fatalf("successful keyless health: %+v", h)
			}
		})
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
			c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-secret", Source: "inline"}})
			c.SetCredential(config.EmbeddingCredential{Key: "example-secret", Source: "file:example.key"})
			_, err := c.Embed(context.Background(), []string{"query"})
			var ce *CredentialError
			var ae *APIError
			wantReason := "rejected the API key"
			if status == 403 {
				wantReason = "provider denied access (403)"
			}
			if !errors.As(err, &ce) || !errors.As(err, &ae) || ae.StatusCode != status || strings.Contains(err.Error(), "example-secret") || !strings.Contains(err.Error(), wantReason) {
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
			c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2, Credential: config.EmbeddingCredential{Key: "old-secret", Source: "inline"}})
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
	c, _ := New(Config{BaseURL: "https://embedding.example", Model: "m", Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	if _, err := c.Embed(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if c.CredentialHealth().LastSuccessAt != nil {
		t.Fatal("empty call reported provider success")
	}
}
