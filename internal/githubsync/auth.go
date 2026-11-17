package githubsync

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"unicode"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/golang-jwt/jwt/v4"

	"go.kenn.io/kata/internal/config"
)

// CredentialKind identifies the credential source selected for a GitHub sync
// binding.
type CredentialKind string

const (
	// CredentialKindApp selects a matching GitHub App installation credential.
	CredentialKindApp CredentialKind = "app"
	// CredentialKindEnv selects the configured environment token credential.
	CredentialKindEnv CredentialKind = "env"
	// CredentialKindGH selects the local gh auth token fallback credential.
	CredentialKindGH CredentialKind = "gh"
)

// CredentialResolver resolves daemon-owned GitHub API credentials for one
// repository binding.
type CredentialResolver struct {
	cfg           config.GitHubSyncConfig
	runner        CommandRunner
	baseTransport http.RoundTripper
	appMu         sync.Mutex
	apps          map[appTransportKey]*ghinstallation.Transport
}

type appTransportKey struct {
	app     config.GitHubAppConfig
	binding Binding
	base    http.RoundTripper
}

// NewCredentialResolver returns a resolver using cfg and runner. A nil runner
// falls back to the real gh command runner.
func NewCredentialResolver(cfg config.GitHubSyncConfig, runner CommandRunner) *CredentialResolver {
	if runner == nil {
		runner = execCommandRunner{}
	}
	return &CredentialResolver{
		cfg:           cfg,
		runner:        runner,
		baseTransport: http.DefaultTransport,
		apps:          make(map[appTransportKey]*ghinstallation.Transport),
	}
}

// ResolveKind reports which credential source would be used for binding.
func (r *CredentialResolver) ResolveKind(ctx context.Context, binding Binding) (CredentialKind, error) {
	credential, err := r.resolve(ctx, binding)
	if err != nil {
		return "", err
	}
	return credential.kind, nil
}

// Transport resolves credentials for binding and wraps base with the selected
// GitHub credential transport.
func (r *CredentialResolver) Transport(ctx context.Context, binding Binding, base http.RoundTripper) (http.RoundTripper, error) {
	tr, _, err := r.credentialTransport(ctx, binding, base)
	return tr, err
}

func (r *CredentialResolver) transport(ctx context.Context, binding Binding) (http.RoundTripper, CredentialKind, error) {
	return r.credentialTransport(ctx, binding, r.nextTransport())
}

func (r *CredentialResolver) credentialTransport(ctx context.Context, binding Binding, base http.RoundTripper) (http.RoundTripper, CredentialKind, error) {
	credential, err := r.resolve(ctx, binding)
	if err != nil {
		return nil, "", err
	}
	base = nextRoundTripper(base)
	switch credential.kind {
	case CredentialKindApp:
		tr, err := r.appTransport(credential.app, credential.binding, base)
		if err != nil {
			return nil, "", fmt.Errorf("create GitHub App transport: %w", err)
		}
		return newOriginGuardTransport(credential.binding, tr), credential.kind, nil
	case CredentialKindEnv, CredentialKindGH:
		return newOriginGuardTransport(credential.binding, &staticBearerTransport{
			token: credential.token,
			next:  base,
		}), credential.kind, nil
	default:
		return nil, "", fmt.Errorf("unsupported GitHub credential kind %q", credential.kind)
	}
}

// appTransport returns the cached transport for app, binding, and base, so sessions
// reuse its installation token until it nears expiry. A base that cannot be a
// map key gets a transport of its own.
func (r *CredentialResolver) appTransport(app config.GitHubAppConfig, binding Binding, base http.RoundTripper) (http.RoundTripper, error) {
	key := appTransportKey{app: app, binding: binding, base: base}
	cacheable := reflect.ValueOf(base).Comparable()
	if cacheable {
		r.appMu.Lock()
		defer r.appMu.Unlock()
		if tr, ok := r.apps[key]; ok {
			return &evictOnUnauthorizedTransport{next: tr, resolver: r, key: key}, nil
		}
	}
	signer := keyFileSigner{path: app.PrivateKeyPath}
	if _, err := signer.readKey(); err != nil {
		return nil, err
	}
	atr, err := ghinstallation.NewAppsTransportWithOptions(base, app.AppID, ghinstallation.WithSigner(signer))
	if err != nil {
		return nil, err
	}
	tr := ghinstallation.NewFromAppsTransport(atr, app.InstallationID)
	tr.BaseURL = githubRESTBaseURL(binding)
	if !cacheable {
		return tr, nil
	}
	r.apps[key] = tr
	return &evictOnUnauthorizedTransport{next: tr, resolver: r, key: key}, nil
}

// keyFileSigner reads the App's private key each time a token is minted, so a
// replaced key file takes effect at the next mint without a restart, and a
// missing file does not fail sessions that a cached token still serves.
type keyFileSigner struct {
	path string
}

func (s keyFileSigner) Sign(claims jwt.Claims) (string, error) {
	key, err := s.readKey()
	if err != nil {
		return "", err
	}
	return ghinstallation.NewRSASigner(jwt.SigningMethodRS256, key).Sign(claims)
}

func (s keyFileSigner) readKey() (*rsa.PrivateKey, error) {
	pemBytes, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read GitHub App private key: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	return key, nil
}

// evictOnUnauthorizedTransport drops a cached App transport when GitHub answers
// 401, so the next session mints a replacement for a revoked token. The
// rejected request is not replayed.
type evictOnUnauthorizedTransport struct {
	next     *ghinstallation.Transport
	resolver *CredentialResolver
	key      appTransportKey
}

func (t *evictOnUnauthorizedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.resolver.appMu.Lock()
		if t.resolver.apps[t.key] == t.next {
			delete(t.resolver.apps, t.key)
		}
		t.resolver.appMu.Unlock()
	}
	return resp, err
}

type resolvedCredential struct {
	binding Binding
	kind    CredentialKind
	app     config.GitHubAppConfig
	token   string
}

func (r *CredentialResolver) resolve(ctx context.Context, binding Binding) (resolvedCredential, error) {
	binding, err := normalizeBinding(binding)
	if err != nil {
		return resolvedCredential{}, err
	}
	if app, ok := r.matchingApp(binding); ok {
		return resolvedCredential{binding: binding, kind: CredentialKindApp, app: app}, nil
	}
	if token, ok := r.envToken(binding); ok {
		return resolvedCredential{binding: binding, kind: CredentialKindEnv, token: token}, nil
	}
	if token, ok, err := r.ghAuthToken(ctx, binding.Host); err != nil {
		return resolvedCredential{}, err
	} else if ok {
		return resolvedCredential{binding: binding, kind: CredentialKindGH, token: token}, nil
	}
	return resolvedCredential{}, r.noCredentialsError(binding)
}

func (r *CredentialResolver) envToken(binding Binding) (string, bool) {
	if binding.Host != r.cfg.TokenHostName() {
		return "", false
	}
	token := strings.TrimSpace(os.Getenv(r.cfg.TokenEnvName()))
	return token, token != ""
}

func (r *CredentialResolver) matchingApp(binding Binding) (config.GitHubAppConfig, bool) {
	wantHost := normalizedAppHost(binding.Host)
	wantOwner := strings.ToLower(strings.TrimSpace(binding.Owner))
	for _, raw := range r.cfg.Apps {
		app := normalizeAppConfig(raw)
		if app.Host == wantHost && strings.ToLower(app.Owner) == wantOwner {
			return app, true
		}
	}
	return config.GitHubAppConfig{}, false
}

func (r *CredentialResolver) ghAuthToken(ctx context.Context, host string) (string, bool, error) {
	stdout, _, err := r.runner.Run(ctx, "gh", "auth", "token", "--hostname", host)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		return "", false, nil
	}
	token := strings.TrimSpace(string(stdout))
	return token, token != "", nil
}

func (r *CredentialResolver) noCredentialsError(binding Binding) error {
	return fmt.Errorf(
		"no GitHub credentials for %s/%s/%s: configure [[github_sync.app]], set %s with [github_sync].token_host = %q, or run `gh auth login --hostname %s`",
		binding.Host,
		binding.Owner,
		binding.Repo,
		r.cfg.TokenEnvName(),
		binding.Host,
		binding.Host,
	)
}

func (r *CredentialResolver) nextTransport() http.RoundTripper {
	if r.baseTransport != nil {
		return r.baseTransport
	}
	return http.DefaultTransport
}

func normalizeAppConfig(app config.GitHubAppConfig) config.GitHubAppConfig {
	app.Host = normalizedAppHost(app.Host)
	app.Owner = strings.TrimSpace(app.Owner)
	app.PrivateKeyPath = strings.TrimSpace(app.PrivateKeyPath)
	return app
}

func normalizedAppHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "github.com"
	}
	return host
}

func githubRESTBaseURL(binding Binding) string {
	if binding.Host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + binding.Host + "/api/v3"
}

type staticBearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t *staticBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("GitHub credential transport received nil request")
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return nextRoundTripper(t.next).RoundTrip(clone)
}

type originGuardTransport struct {
	binding Binding
	next    http.RoundTripper
}

func newOriginGuardTransport(binding Binding, next http.RoundTripper) http.RoundTripper {
	return &originGuardTransport{binding: binding, next: nextRoundTripper(next)}
}

func (t *originGuardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("GitHub credential egress blocked: nil request URL")
	}
	binding, err := normalizeBinding(t.binding)
	if err != nil {
		return nil, err
	}
	if ok, err := allowedGitHubAPIRequest(binding, req); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("GitHub credential egress blocked: refusing %s for %s/%s/%s",
			req.URL.Redacted(), binding.Host, binding.Owner, binding.Repo)
	}
	return nextRoundTripper(t.next).RoundTrip(req)
}

func nextRoundTripper(next http.RoundTripper) http.RoundTripper {
	if next != nil {
		return next
	}
	return http.DefaultTransport
}

func allowedGitHubAPIRequest(binding Binding, req *http.Request) (bool, error) {
	if req == nil {
		return false, nil
	}
	isGraphQL, ok := allowedGitHubAPIURL(binding, req.URL)
	if !ok {
		return false, nil
	}
	if !isGraphQL {
		return true, nil
	}
	return scopedGraphQLRequest(req, binding)
}

func allowedGitHubAPIURL(binding Binding, u *url.URL) (bool, bool) {
	if u == nil || u.Scheme != "https" {
		return false, false
	}
	segments, ok := decodedPathSegments(u)
	if !ok {
		return false, false
	}
	if binding.Host == "github.com" {
		if !strings.EqualFold(u.Host, "api.github.com") {
			return false, false
		}
		if isGitHubGraphQLPath(segments) {
			return true, true
		}
		return false, isGitHubRESTPath(segments, 0, binding)
	}
	if !strings.EqualFold(u.Host, binding.Host) {
		return false, false
	}
	if isEnterpriseGraphQLPath(segments) {
		return true, true
	}
	return false, isGitHubRESTPath(segments, 2, binding)
}

func decodedPathSegments(u *url.URL) ([]string, bool) {
	path := strings.Trim(u.EscapedPath(), "/")
	if path == "" {
		return nil, true
	}
	rawSegments := strings.Split(path, "/")
	segments := make([]string, len(rawSegments))
	for i, raw := range rawSegments {
		segment, err := url.PathUnescape(raw)
		if err != nil || segment == "" || segment == "." || segment == ".." || strings.Contains(segment, "/") {
			return nil, false
		}
		segments[i] = segment
	}
	return segments, true
}

func isGitHubGraphQLPath(segments []string) bool {
	return len(segments) == 1 && segments[0] == "graphql"
}

func isEnterpriseGraphQLPath(segments []string) bool {
	return len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql"
}

func isGitHubRESTPath(segments []string, offset int, binding Binding) bool {
	if len(segments) < offset+3 {
		return false
	}
	if offset == 2 && (segments[0] != "api" || segments[1] != "v3") {
		return false
	}
	return segments[offset] == "repos" &&
		strings.EqualFold(segments[offset+1], binding.Owner) &&
		strings.EqualFold(segments[offset+2], binding.Repo)
}

type graphQLRequestBody struct {
	Query     string                    `json:"query"`
	Variables map[string]jsontext.Value `json:"variables"`
}

func scopedGraphQLRequest(req *http.Request, binding Binding) (bool, error) {
	if req.Method != http.MethodPost {
		return false, nil
	}
	body, err := readAndRestoreRequestBody(req)
	if err != nil {
		return false, fmt.Errorf("GitHub credential egress blocked: read GraphQL request body: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false, nil
	}
	var payload graphQLRequestBody
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, nil
	}
	owner, ok := graphQLStringVariable(payload.Variables, "owner")
	if !ok || owner != binding.Owner {
		return false, nil
	}
	repo, ok := graphQLStringVariable(payload.Variables, "repo")
	if !ok || repo != binding.Repo {
		return false, nil
	}
	return graphQLQueryMatchesParentQuery(payload.Query), nil
}

func readAndRestoreRequestBody(req *http.Request) ([]byte, error) {
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer func() { _ = body.Close() }()
		return io.ReadAll(body)
	}
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if err := req.Body.Close(); err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func graphQLStringVariable(variables map[string]jsontext.Value, name string) (string, bool) {
	raw, ok := variables[name]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", false
	}
	return value, true
}

func graphQLQueryMatchesParentQuery(query string) bool {
	return compactGraphQLQuery(query) == compactGraphQLQuery(parentGraphQLQuery) || matchesTargetedQuery(query, targetedParentGraphQLQuery) || matchesTargetedQuery(query, targetedStatusGraphQLQuery)
}

func compactGraphQLQuery(query string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, query)
}
