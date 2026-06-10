package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

// catalog builds a minimal daemon catalog for resolver tests.
func catalog(entries ...config.CatalogDaemonConfig) *config.DaemonConfig {
	return &config.DaemonConfig{Daemons: entries}
}

// TestResolveHubAdminAuthExplicitTokenKeepsBindingURL asserts an explicit
// --hub-token wins and the target URL stays the binding's hub URL.
func TestResolveHubAdminAuthExplicitTokenKeepsBindingURL(t *testing.T) {
	out, err := resolveHubAdminAuth(catalog(config.CatalogDaemonConfig{
		Name: "hub", URL: "https://other.example", Token: "catalog-token",
	}), hubAuthInputs{hubURL: "https://bound.example", hubName: "hub", hubToken: "explicit"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out.url != "https://bound.example" {
		t.Fatalf("url should be the binding hub URL, got %q", out.url)
	}
	if out.token != "explicit" {
		t.Fatalf("token should be the explicit --hub-token, got %q", out.token)
	}
}

// TestResolveHubAdminAuthNamedEntryKeepsBindingURL is the core Fix 1 case: a
// --hub <name> catalog entry supplies ONLY the token; the target URL must
// remain the binding's hub URL even though the catalog entry has a different
// URL, so the admin token is never sent to a foreign origin.
func TestResolveHubAdminAuthNamedEntryKeepsBindingURL(t *testing.T) {
	out, err := resolveHubAdminAuth(catalog(config.CatalogDaemonConfig{
		Name: "hub", URL: "https://attacker.example", Token: "catalog-token",
	}), hubAuthInputs{hubURL: "https://bound.example", hubName: "hub"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out.url != "https://bound.example" {
		t.Fatalf("url must stay the binding hub URL, got %q", out.url)
	}
	if out.token != "catalog-token" {
		t.Fatalf("token should come from the catalog entry, got %q", out.token)
	}
}

// TestResolveHubAdminAuthURLMatchToleratesTrailingSlash asserts the catalog
// URL match normalizes trailing slashes on both sides (#273).
func TestResolveHubAdminAuthURLMatchToleratesTrailingSlash(t *testing.T) {
	out, err := resolveHubAdminAuth(catalog(config.CatalogDaemonConfig{
		Name: "hub", URL: "https://bound.example/", Token: "catalog-token",
	}), hubAuthInputs{hubURL: "https://bound.example"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out.token != "catalog-token" {
		t.Fatalf("trailing-slash URL should still match, got token %q", out.token)
	}
	if out.url != "https://bound.example" {
		t.Fatalf("url should be the normalized binding URL, got %q", out.url)
	}
}

// TestResolveHubAdminAuthUsesBindingAllowInsecure asserts allow_insecure for
// the hub client comes from the binding, not the catalog entry.
func TestResolveHubAdminAuthUsesBindingAllowInsecure(t *testing.T) {
	out, err := resolveHubAdminAuth(catalog(config.CatalogDaemonConfig{
		Name: "hub", URL: "https://bound.example", Token: "catalog-token", AllowInsecure: false,
	}), hubAuthInputs{hubURL: "https://bound.example", hubName: "hub", allowInsecure: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !out.allowInsecure {
		t.Fatalf("allowInsecure should come from the binding (true), got %v", out.allowInsecure)
	}
}

// TestResolveHubAdminAuthNoEntryGlobalFallback asserts the no-entry case yields
// an empty token (the caller then falls back to global auth) with no error.
func TestResolveHubAdminAuthNoEntryGlobalFallback(t *testing.T) {
	out, err := resolveHubAdminAuth(catalog(), hubAuthInputs{hubURL: "https://bound.example"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out.token != "" {
		t.Fatalf("no matching entry should yield empty token, got %q", out.token)
	}
	if out.url != "https://bound.example" {
		t.Fatalf("url should be the binding URL, got %q", out.url)
	}
}

// TestResolveHubAdminAuthSelectedTokenEnvUnsetErrors is Fix 2: when a SELECTED
// catalog entry (by name) has token_env set but the env var is empty, resolving
// must error rather than silently fall back to global daemon auth.
func TestResolveHubAdminAuthSelectedTokenEnvUnsetErrors(t *testing.T) {
	t.Setenv("KATA_TEST_MISSING_HUB_TOKEN", "")
	_, err := resolveHubAdminAuth(catalog(config.CatalogDaemonConfig{
		Name: "hub", URL: "https://bound.example", TokenEnv: "KATA_TEST_MISSING_HUB_TOKEN",
	}), hubAuthInputs{hubURL: "https://bound.example", hubName: "hub"})
	if err == nil {
		t.Fatalf("expected an error when selected entry's token_env is unset")
	}
}

// TestResolveHubAdminAuthURLMatchTokenEnvUnsetErrors is Fix 2 for the URL-match
// selection branch: a URL-matched entry with an empty token_env also errors.
func TestResolveHubAdminAuthURLMatchTokenEnvUnsetErrors(t *testing.T) {
	t.Setenv("KATA_TEST_MISSING_HUB_TOKEN", "")
	_, err := resolveHubAdminAuth(catalog(config.CatalogDaemonConfig{
		Name: "hub", URL: "https://bound.example", TokenEnv: "KATA_TEST_MISSING_HUB_TOKEN",
	}), hubAuthInputs{hubURL: "https://bound.example"})
	if err == nil {
		t.Fatalf("expected an error when URL-matched entry's token_env is unset")
	}
}

// TestResolveHubAdminAuthSelectedTokenEnvSetSucceeds confirms a populated
// token_env on a selected entry resolves to that token.
func TestResolveHubAdminAuthSelectedTokenEnvSetSucceeds(t *testing.T) {
	t.Setenv("KATA_TEST_HUB_TOKEN", "env-token")
	out, err := resolveHubAdminAuth(catalog(config.CatalogDaemonConfig{
		Name: "hub", URL: "https://bound.example", TokenEnv: "KATA_TEST_HUB_TOKEN",
	}), hubAuthInputs{hubURL: "https://bound.example", hubName: "hub"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out.token != "env-token" {
		t.Fatalf("token should come from token_env, got %q", out.token)
	}
}

// TestHubAdminClientNeverSendsGlobalTokenWithoutHubCredential: when no
// hub-specific credential resolves, the hub client must be unauthenticated —
// the local daemon's global bearer token must not leak to the hub origin.
func TestHubAdminClientNeverSendsGlobalTokenWithoutHubCredential(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "local-daemon-secret")
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hc, err := hubAdminClient(context.Background(), hubAdminAuth{url: srv.URL})
	require.NoError(t, err)
	resp, err := hc.Get(srv.URL + "/api/v1/federation/enrollments")
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Empty(t, got, "global daemon token must not be sent to the hub origin")
}
