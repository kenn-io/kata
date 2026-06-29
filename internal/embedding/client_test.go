package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newFakeServer(t *testing.T, status int, body string, retryAfter string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestEmbedNormalizesVectors(t *testing.T) {
	srv := newFakeServer(t, 200, `{"data":[{"embedding":[3,4]}]}`, "")
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := c.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	// [3,4] normalized is [0.6,0.8].
	if math.Abs(float64(vecs[0][0])-0.6) > 1e-6 || math.Abs(float64(vecs[0][1])-0.8) > 1e-6 {
		t.Fatalf("not normalized: %v", vecs[0])
	}
}

func TestEmbedDimsMismatchErrors(t *testing.T) {
	srv := newFakeServer(t, 200, `{"data":[{"embedding":[1,2,3]}]}`, "")
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected dims-mismatch error")
	}
}

func TestEmbed401IsDefinitive(t *testing.T) {
	srv := newFakeServer(t, 401, `{"error":"bad key"}`, "")
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	_, err := c.Embed(context.Background(), []string{"x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Definitive() {
		t.Fatalf("want definitive APIError, got %v", err)
	}
}

func TestEmbed429CarriesRetryAfter(t *testing.T) {
	srv := newFakeServer(t, 429, `{}`, "7")
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	_, err := c.Embed(context.Background(), []string{"x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.Definitive() {
		t.Fatal("429 must not be definitive")
	}
	if apiErr.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", apiErr.RetryAfter)
	}
}

func TestEmbed429RetryAfterHTTPDate(t *testing.T) {
	// A future HTTP-date Retry-After must be converted to a positive delay,
	// bounded by the offset (plus a little slack for the round trip).
	const offset = 30 * time.Second
	future := time.Now().UTC().Add(offset).Format(http.TimeFormat)
	srv := newFakeServer(t, 429, `{}`, future)
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	_, err := c.Embed(context.Background(), []string{"x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want a positive delay from the HTTP-date", apiErr.RetryAfter)
	}
	// http.TimeFormat has 1-second resolution, so the parsed instant can sit up
	// to a second beyond `offset`; add slack for that plus request latency.
	if apiErr.RetryAfter > offset+5*time.Second {
		t.Fatalf("RetryAfter = %v, want <= %v", apiErr.RetryAfter, offset+5*time.Second)
	}
}

func TestEmbed429RetryAfterPastHTTPDateClampsToZero(t *testing.T) {
	// A past HTTP-date must clamp to 0, never a negative backoff.
	past := time.Now().UTC().Add(-1 * time.Hour).Format(http.TimeFormat)
	srv := newFakeServer(t, 429, `{}`, past)
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	_, err := c.Embed(context.Background(), []string{"x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0 (past date clamped)", apiErr.RetryAfter)
	}
}

func TestEmbedBatchesPreserveOrder(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		calls++
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// Encode each input's position into a distinct raw vector so the test
		// can verify global ordering even though the server sees only one batch
		// per call. Input "n" -> raw vector [n+1, 1].
		data := make([]map[string]any, len(req.Input))
		for i, s := range req.Input {
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Errorf("input %q not an integer: %v", s, err)
			}
			data[i] = map[string]any{"embedding": []float32{float32(n + 1), 1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	inputs := []string{"0", "1", "2", "3", "4"}
	vecs, err := c.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != len(inputs) {
		t.Fatalf("got %d vectors, want %d", len(vecs), len(inputs))
	}
	// 5 inputs at BatchSize 2 -> ceil(5/2) = 3 HTTP calls.
	if calls != 3 {
		t.Fatalf("server received %d calls, want 3", calls)
	}
	for i := range inputs {
		want := normalize([]float32{float32(i + 1), 1})
		if math.Abs(float64(vecs[i][0]-want[0])) > 1e-6 || math.Abs(float64(vecs[i][1]-want[1])) > 1e-6 {
			t.Fatalf("vec[%d] = %v, want %v (out of order or wrong batch)", i, vecs[i], want)
		}
	}
}

func TestEmbedKeyOnlyToConfiguredOrigin(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{1, 0}}}})
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dims: 2, APIKey: "secret"})
	if _, err := c.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
}

func TestNewRejectsUnsafeBaseURLWithoutAPIKey(t *testing.T) {
	_, err := New(Config{BaseURL: "http://example.com/v1", Model: "m", Dims: 2})
	if err == nil {
		t.Fatal("expected unsafe plaintext public URL to be rejected without an API key")
	}
}

func TestEmbedRejectsCrossOriginRedirectWithoutAPIKey(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{1, 0}}}})
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/embeddings", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c, err := New(Config{BaseURL: redirector.URL, Model: "m", Dims: 2})
	if err != nil {
		t.Fatalf("new embedding client: %v", err)
	}
	_, err = c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected cross-origin redirect to be rejected without an API key")
	}
	if redirected {
		t.Fatal("embedding payload followed cross-origin redirect")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Fatalf("error = %v, want origin context", err)
	}
}
