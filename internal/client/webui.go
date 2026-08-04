package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	kitdaemon "go.kenn.io/kit/daemon"
)

var errRemoteWebUILoginRequired = errors.New("remote web UI login required")

// PrepareWebUIOptions selects the same daemon source as other CLI commands.
type PrepareWebUIOptions struct {
	WorkspaceStart string
	DaemonName     string
}

// PreparedWebUI is a validated daemon target ready for resolution and launch.
// Runtime is populated only for a local target; configured remotes use login.
type PreparedWebUI struct {
	BaseURL             string
	ConfiguredRemote    bool
	AllowInsecure       bool
	TrustPrivateNetwork bool
	Client              *http.Client
	AnonymousClient     *http.Client
	Runtime             DiscoveredWebRuntime
}

// WebUILaunch is the validated browser target selected by OpenWebUI.
type WebUILaunch struct {
	PublicURL string
}

// WebUIOpener hands a credential-safe launch request to the platform browser
// launcher.
type WebUIOpener func(context.Context, WebUILaunch) error

// PrepareWebUI resolves and probes the selected daemon and validates local
// browser runtime metadata.
func PrepareWebUI(ctx context.Context, opts PrepareWebUIOptions) (PreparedWebUI, error) {
	var (
		baseURL          string
		configuredRemote bool
		allowInsecure    bool
	)
	if opts.DaemonName != "" {
		target, err := resolveNamedDaemonTarget(ctx, opts.DaemonName)
		if err != nil {
			return PreparedWebUI{}, err
		}
		baseURL = target.BaseURL
		configuredRemote = !target.Local
		allowInsecure = target.AllowInsecure
	} else {
		target, err := ensureRunningTargetInWorkspace(ctx, opts.WorkspaceStart)
		if err != nil {
			return PreparedWebUI{}, err
		}
		baseURL = target.BaseURL
		configuredRemote = target.ConfiguredRemote
		allowInsecure = remoteAllowInsecureForBaseURL(baseURL, opts.WorkspaceStart)
	}

	httpClient, err := NewHTTPClient(ctx, baseURL, Opts{
		Timeout:        DefaultHTTPTimeout,
		AllowInsecure:  allowInsecure,
		WorkspaceStart: opts.WorkspaceStart,
		DaemonName:     opts.DaemonName,
	})
	if err != nil {
		return PreparedWebUI{}, err
	}
	prepared := PreparedWebUI{
		BaseURL: baseURL, ConfiguredRemote: configuredRemote, AllowInsecure: allowInsecure,
		TrustPrivateNetwork: resolveAuthConfig().TrustPrivateNetwork, Client: httpClient,
	}
	if configuredRemote {
		if err := validateWebLoginTarget(baseURL, prepared.TrustPrivateNetwork, allowInsecure); err != nil {
			return PreparedWebUI{}, err
		}
		if _, err := validatedWebBaseURL(baseURL, true); err != nil {
			return PreparedWebUI{}, err
		}
		anonymousClient, err := NewHTTPClientForTarget(ctx, baseURL, TargetAuth{}, Opts{
			Timeout: DefaultHTTPTimeout,
		})
		if err != nil {
			return PreparedWebUI{}, err
		}
		prepared.AnonymousClient = anonymousClient
		return prepared, nil
	}

	namespace, err := daemon.NewNamespace()
	if err != nil {
		return PreparedWebUI{}, err
	}
	runtimeInfo, err := discoverWebRuntimeForBaseURL(ctx, namespace.DataDir, baseURL)
	if err != nil {
		return PreparedWebUI{}, err
	}
	prepared.Runtime = runtimeInfo
	return prepared, nil
}

func discoverWebRuntimeForBaseURL(ctx context.Context, dataDir, baseURL string) (DiscoveredWebRuntime, error) {
	records, err := (kitdaemon.RuntimeStore{Dir: dataDir}).List()
	if err != nil {
		return DiscoveredWebRuntime{}, fmt.Errorf("list daemon runtime records: %w", err)
	}
	for _, record := range records {
		if !kitdaemon.ProcessAlive(record.PID) {
			continue
		}
		candidate, _, ok := probeAddress(ctx, record.Endpoint().ConfigAddress())
		if !ok || candidate != baseURL {
			continue
		}
		discovered, err := DiscoverWebRuntime(record)
		if err != nil {
			return DiscoveredWebRuntime{}, fmt.Errorf("validate browser runtime metadata: %w", err)
		}
		return discovered, nil
	}
	return DiscoveredWebRuntime{}, errors.New("live daemon did not publish matching browser runtime metadata")
}

// OpenWebUI opens keyless loopback targets directly and selects login for
// authenticated browser listeners.
func OpenWebUI(
	ctx context.Context,
	prepared PreparedWebUI,
	returnPath string,
	opener WebUIOpener,
) error {
	if opener == nil {
		return errors.New("web UI opener is required")
	}
	normalized, err := normalizeWebUIReturnPath(returnPath)
	if err != nil {
		return err
	}
	if prepared.ConfiguredRemote {
		if err := validateWebLoginTarget(
			prepared.BaseURL, prepared.TrustPrivateNetwork, prepared.AllowInsecure,
		); err != nil {
			return err
		}
		base, err := validatedWebBaseURL(prepared.BaseURL, false)
		if err != nil {
			return err
		}
		anonymousOrigin, err := probeAnonymousReadonlyWebUI(
			ctx, prepared.AnonymousClient, base, prepared.TrustPrivateNetwork, prepared.AllowInsecure,
		)
		if errors.Is(err, errRemoteWebUILoginRequired) {
			if anonymousOrigin == nil {
				return errors.New("remote daemon did not advertise a usable canonical browser origin")
			}
			target := webLaunchURL(anonymousOrigin)
			target += "#" + url.Values{"login": {"1"}, "return_path": {normalized}}.Encode()
			return opener(ctx, WebUILaunch{PublicURL: target})
		}
		if err != nil {
			return err
		}
		if anonymousOrigin == nil {
			return errors.New("remote daemon did not advertise a usable browser origin")
		}
		return opener(ctx, WebUILaunch{PublicURL: webLaunchURLAt(anonymousOrigin, normalized)})
	}
	if err := validateLocalWebRuntime(prepared.Runtime); err != nil {
		return err
	}
	base, err := validatedWebBaseURL(prepared.Runtime.Origin, false)
	if err != nil {
		return err
	}
	if runtimeHasCapability(prepared.Runtime, "loopback") ||
		runtimeHasCapability(prepared.Runtime, "readonly") {
		return opener(ctx, WebUILaunch{PublicURL: webLaunchURLAt(base, normalized)})
	}
	target := webLaunchURL(base)
	target += "#" + url.Values{"login": {"1"}, "return_path": {normalized}}.Encode()
	return opener(ctx, WebUILaunch{PublicURL: target})
}

func probeAnonymousReadonlyWebUI(
	ctx context.Context,
	client *http.Client,
	base *url.URL,
	trustPrivateNetwork, allowInsecure bool,
) (*url.URL, error) {
	if client == nil {
		return nil, errors.New("remote web UI probe requires an anonymous HTTP client")
	}
	target := *base
	target.Path = "/api/v1/ui/snapshot"
	target.RawQuery = url.Values{
		"view": {"all-open"}, "include_graph": {"false"},
		"include_history": {"false"}, "limit": {"1"},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build anonymous web UI probe: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	probeClient := *client
	probeClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := probeClient.Do(request) //nolint:gosec // Target is the validated configured daemon URL.
	if err != nil {
		return nil, fmt.Errorf("probe anonymous web UI: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
		canonicalOrigin := response.Header.Get(daemon.WebOriginHeader)
		if canonicalOrigin == "" {
			return nil, errors.New("remote daemon did not advertise its canonical browser origin")
		}
		if err := validateWebLoginTarget(canonicalOrigin, trustPrivateNetwork, allowInsecure); err != nil {
			return nil, err
		}
		origin, err := validatedWebBaseURL(canonicalOrigin, false)
		if err != nil {
			return nil, err
		}
		if response.Header.Get(daemon.WebAuthenticationHeader) == "loopback" {
			if !isLoopbackWebOrigin(origin) {
				return nil, errors.New("remote daemon advertised loopback authentication on a non-loopback origin")
			}
			return origin, nil
		}
		return origin, errRemoteWebUILoginRequired
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
		return nil, fmt.Errorf("probe anonymous web UI returned HTTP %d", response.StatusCode)
	}
	var snapshot struct {
		Capabilities api.UICapabilities `json:"capabilities"`
		Origin       string             `json:"origin"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, errors.New("anonymous web UI snapshot was invalid")
	}
	if snapshot.Capabilities.Writable || snapshot.Capabilities.Updates != "poll" {
		return nil, errors.New("remote daemon does not advertise anonymous read-only web access")
	}
	if err := validateWebLoginTarget(snapshot.Origin, trustPrivateNetwork, allowInsecure); err != nil {
		return nil, err
	}
	origin, err := validatedWebBaseURL(snapshot.Origin, false)
	if err != nil {
		return nil, err
	}
	return origin, nil
}

func validateWebLoginTarget(baseURL string, trustPrivateNetwork, allowInsecure bool) error {
	if allowInsecure {
		_, err := config.BearerOriginForBaseURLAllowInsecure(baseURL)
		return err
	}
	_, err := checkBearerTargetSafe(baseURL, trustPrivateNetwork)
	return err
}

func isLoopbackWebOrigin(origin *url.URL) bool {
	host := origin.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func runtimeHasCapability(runtimeInfo DiscoveredWebRuntime, wanted string) bool {
	for _, capability := range runtimeInfo.Capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func validateLocalWebRuntime(runtimeInfo DiscoveredWebRuntime) error {
	if _, err := validatedWebBaseURL(runtimeInfo.Origin, false); err != nil {
		return err
	}
	if runtimeHasCapability(runtimeInfo, "loopback") ||
		runtimeHasCapability(runtimeInfo, "readonly") ||
		runtimeHasCapability(runtimeInfo, "login") {
		return nil
	}
	return errors.New("browser runtime does not advertise a supported authentication mode")
}

func validatedWebBaseURL(value string, allowPath bool) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("browser origin metadata is invalid")
	}
	if !allowPath && parsed.Path != "" {
		return nil, errors.New("browser target URL path prefixes are unsupported")
	}
	if allowPath && parsed.Path != "" &&
		(strings.Contains(parsed.Path, `\`) || path.Clean(parsed.Path) != parsed.Path) {
		return nil, errors.New("browser target URL is invalid")
	}
	return parsed, nil
}

func normalizeWebUIReturnPath(value string) (string, error) {
	if value == "" {
		return "/kata", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil ||
		parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		return "", errors.New("web UI return path must be a same-origin absolute path")
	}
	return parsed.RequestURI(), nil
}

func webLaunchURL(base *url.URL) string {
	return webLaunchURLAt(base, "/kata")
}

func webLaunchURLAt(base *url.URL, returnPath string) string {
	launchURL := *base
	targetPath, rawQuery, _ := strings.Cut(returnPath, "?")
	launchURL.Path = strings.TrimRight(launchURL.Path, "/") + targetPath
	launchURL.RawPath = ""
	launchURL.RawQuery = rawQuery
	launchURL.Fragment = ""
	launchURL.RawFragment = ""
	return launchURL.String()
}
