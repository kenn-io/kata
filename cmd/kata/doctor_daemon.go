package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/diagnostics"
	"go.kenn.io/kata/internal/version"
	kataapi "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func (s *doctorState) daemonChecks(ctx context.Context) {
	var apiClient *kataapi.Client
	var completeProjectCatalog bool
	s.add("daemon.connection", "daemon", func() diagnostics.Check {
		injectedURL, hasInjectedURL := ctx.Value(client.BaseURLKey{}).(string)
		hasInjectedURL = hasInjectedURL && injectedURL != ""
		hasServerEnv := os.Getenv("KATA_SERVER") != ""
		namedDaemon := flags.Daemon
		if !s.workspaceValid && namedDaemon == "" && !hasInjectedURL && !hasServerEnv {
			cfg, err := config.ReadDaemonConfig()
			if err != nil {
				return diagnostics.Check{Status: "fail", Summary: "Selected daemon configuration could not be read", Fix: "Check <KATA_HOME>/config.toml and its active_daemon entry."}
			}
			if cfg != nil {
				namedDaemon = cfg.ActiveDaemon
			}
		}
		if !s.workspaceValid && namedDaemon == "" && !hasInjectedURL && !hasServerEnv {
			return skippedDoctorCheck("Workspace resolution failed")
		}
		probe, cancel := context.WithTimeout(ctx, doctorRequestTimeout)
		defer cancel()
		var err error
		var resolved client.ResolvedDaemon
		if hasInjectedURL {
			resolved, err = client.EnsureResolvedInWorkspace(probe, "")
		} else if namedDaemon != "" {
			resolved, err = client.DiscoverResolvedNamedTargetReadOnly(probe, namedDaemon)
		} else if !s.workspaceValid && hasServerEnv {
			var found bool
			resolved, found, err = client.DiscoverResolvedReadOnlyInWorkspace(probe, "")
			if err == nil && !found {
				err = errors.New("no running daemon")
			}
		} else {
			var found bool
			resolved, found, err = client.DiscoverResolvedReadOnlyInWorkspace(probe, s.workspace)
			if err == nil && !found {
				err = errors.New("no running daemon")
			}
		}
		base := resolved.BaseURL
		if err != nil || base == "" {
			return diagnostics.Check{Status: "fail", Summary: "Selected daemon is unavailable or its target configuration is invalid", Fix: "Check --daemon, KATA_SERVER, .kata.local.toml and active_daemon; for a stopped local daemon, run kata daemon start."}
		}
		hc, err := client.NewHTTPClientForResolved(probe, resolved, client.Opts{Timeout: doctorRequestTimeout})
		if err != nil {
			return diagnostics.Check{Status: "fail", Summary: "Cannot construct a client for the selected daemon", Fix: "Check the selected daemon's credential and transport configuration."}
		}
		// Diagnostics follow no redirects even for uncredentialed requests.
		hc.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
		apiClient, err = newDoctorAPIClient(base, hc)
		if err != nil {
			return diagnostics.Check{Status: "fail", Summary: "Cannot construct a client for the selected daemon", Fix: "Check the selected daemon's credential and transport configuration."}
		}
		ping, err := apiClient.PingWithResponse(probe)
		if err != nil || ping == nil || ping.StatusCode != http.StatusOK || ping.JSON200 == nil || !ping.JSON200.Ok || ping.JSON200.Service != "kata" {
			apiClient = nil
			return diagnostics.Check{Status: "fail", Summary: "Selected endpoint did not identify a healthy Kata service", Fix: "Confirm the configured URL or local runtime record belongs to a Kata daemon."}
		}
		instance, instanceErr := apiClient.InstanceWithResponse(probe)
		instanceVerified := instanceErr == nil && instance != nil && instance.StatusCode == http.StatusOK && instance.JSON200 != nil
		if instanceVerified {
			auth := instance.JSON200.Auth
			completeProjectCatalog = auth.Kind != "" && auth.Scope == nil
		}
		if resolved.LocalProfile != nil {
			if !instanceVerified {
				apiClient = nil
				status := 0
				if instance != nil {
					status = instance.StatusCode
				}
				return diagnostics.Check{Status: "fail", Summary: "Selected local profile identity could not be verified", Details: doctorHTTPStatus(status), Fix: "Check the selected profile's pinned instance_uid and daemon identity."}
			}
			if instance.JSON200.InstanceUID != resolved.LocalProfile.InstanceUID {
				apiClient = nil
				return diagnostics.Check{Status: "fail", Summary: "Selected local profile identity does not match its configured instance_uid", Fix: "Check the selected profile's home and pinned instance_uid; do not substitute another database."}
			}
		}
		return diagnostics.Check{Status: "ok", Summary: "Selected daemon identifies as Kata and is reachable"}
	})
	var health *generated.HealthResponse
	var healthOK bool
	s.add("daemon.health", "daemon", func() diagnostics.Check {
		if apiClient == nil {
			return skippedDoctorCheck("Daemon connection could not be verified")
		}
		response, err := apiClient.HealthWithResponse(ctx)
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		if err != nil || response == nil || status != http.StatusOK || response.JSON200 == nil || !response.JSON200.Ok || response.JSON200.SchemaVersion <= 0 {
			return diagnostics.Check{Status: "fail", Summary: "Selected daemon health could not be verified", Details: doctorHTTPStatus(status), Fix: "Check kata daemon logs on the daemon host and the target credentials; repair daemon/storage health before retrying."}
		}
		health = response.JSON200
		healthOK = true
		return diagnostics.Check{Status: "ok", Summary: "Selected daemon can read its storage schema", Details: []string{fmt.Sprintf("storage schema version: %d", health.SchemaVersion)}}
	})
	s.add("daemon.version", "daemon", func() diagnostics.Check {
		if !healthOK {
			return skippedDoctorCheck("Daemon health is unavailable")
		}
		apiSchemaVersion := ""
		if health.APISchemaVersion != nil {
			apiSchemaVersion = *health.APISchemaVersion
		}
		if health.Version != version.Version || apiSchemaVersion != daemon.APISchemaVersion {
			return diagnostics.Check{Status: "warn", Summary: "CLI and daemon version or API contract differ", Fix: "Align the CLI and selected daemon versions; restart the daemon explicitly after upgrading it."}
		}
		return diagnostics.Check{Status: "ok", Summary: "CLI and daemon version and API contract match"}
	})
	s.add("workspace.project", "workspace", func() diagnostics.Check {
		if apiClient == nil || s.project == "" {
			return skippedDoctorCheck("A verified daemon and a bound or explicit project are required")
		}
		catalog, err := apiClient.ListProjectsWithResponse(ctx, nil)
		status := 0
		if catalog != nil {
			status = catalog.StatusCode
		}
		if err != nil || catalog == nil || status != http.StatusOK || catalog.JSON200 == nil {
			return diagnostics.Check{Status: "warn", Summary: "Project catalog is unavailable or not visible to this principal", Details: doctorHTTPStatus(status), Fix: "Use a credential with project catalog visibility to verify the workspace binding."}
		}
		for _, p := range catalog.JSON200.Projects {
			if p.Name == s.project && p.DeletedAt == nil {
				return diagnostics.Check{Status: "ok", Summary: "Effective project name exists on the selected daemon"}
			}
		}
		if !completeProjectCatalog {
			return diagnostics.Check{Status: "warn", Summary: "Effective project is not visible in the selected daemon's catalog", Fix: "Use a credential with full project catalog visibility to verify the workspace binding."}
		}
		return diagnostics.Check{Status: "fail", Summary: "Effective project name does not match an active project on the selected daemon", Fix: "Check the selected daemon and --project; repair stale or renamed workspace bindings with kata init. Doctor does not resolve aliases or repair bindings."}
	})
	s.add("daemon.embeddings", "integration", func() diagnostics.Check {
		if !healthOK {
			return skippedDoctorCheck("Daemon health is unavailable")
		}
		return doctorEmbeddings(health.Embeddings)
	})
	s.add("daemon.federation", "integration", func() diagnostics.Check {
		if !healthOK {
			return skippedDoctorCheck("Daemon health is unavailable")
		}
		return doctorFederation(health.FederationConfig)
	})
	s.hookChecks(ctx, apiClient)
}

func skippedDoctorCheck(reason string) diagnostics.Check {
	return diagnostics.Check{Status: "info", Summary: "Check skipped: " + reason}
}

const doctorResponseLimit = 8 << 20

var errDoctorResponseTooLarge = errors.New("diagnostic response exceeds size limit")

// The generated API client reads response bodies in full, so cap each body at
// the same limit used by doctor before passing it into the generated decoder.
type doctorResponseLimitTransport struct {
	next http.RoundTripper
}

func (t doctorResponseLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Ask the underlying transport not to decode responses invisibly. Doctor
	// must apply its size budget to the decoded bytes before the generated
	// client buffers them, regardless of transport implementation.
	boundedReq := req.Clone(req.Context())
	boundedReq.Header = req.Header.Clone()
	boundedReq.Header.Set("Accept-Encoding", "identity")
	response, err := t.next.RoundTrip(boundedReq)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("Content-Encoding")), "gzip") {
		compressedBody := response.Body
		decodedBody, decodeErr := gzip.NewReader(compressedBody)
		if decodeErr != nil {
			_ = compressedBody.Close()
			return nil, fmt.Errorf("decode gzip diagnostic response: %w", decodeErr)
		}
		response.Body = &doctorGzipResponseBody{Reader: decodedBody, compressed: compressedBody}
		response.Header.Del("Content-Encoding")
		response.Header.Del("Content-Length")
		response.ContentLength = -1
		response.Uncompressed = true
	}
	response.Body = &doctorResponseLimitBody{ReadCloser: response.Body}
	return response, nil
}

type doctorGzipResponseBody struct {
	*gzip.Reader
	compressed io.Closer
}

func (b *doctorGzipResponseBody) Close() error {
	return errors.Join(b.Reader.Close(), b.compressed.Close())
}

type doctorResponseLimitBody struct {
	io.ReadCloser
	read int64
}

func (b *doctorResponseLimitBody) Read(p []byte) (int, error) {
	remaining := int64(doctorResponseLimit+1) - b.read
	if remaining <= 0 {
		return 0, errDoctorResponseTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	if b.read > doctorResponseLimit {
		return n, errDoctorResponseTooLarge
	}
	return n, err
}

func newDoctorAPIClient(base string, hc *http.Client) (*kataapi.Client, error) {
	if hc == nil {
		return nil, errors.New("doctor HTTP client is nil")
	}
	next := hc.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	hc.Transport = doctorResponseLimitTransport{next: next}
	return kataapi.NewWithHTTPClient(base, hc)
}

func doctorHTTPStatus(status int) []string {
	if status == 0 {
		return nil
	}
	return []string{fmt.Sprintf("HTTP status: %d", status)}
}

func doctorEmbeddings(h *generated.EmbeddingsHealth) diagnostics.Check {
	if h == nil {
		return diagnostics.Check{Status: "info", Summary: "Selected daemon did not provide embedding health details; configuration and provider state are unknown"}
	}
	if !h.Configured {
		return diagnostics.Check{Status: "info", Summary: "Selected daemon does not report configured embeddings; lexical search remains available"}
	}
	credential := ""
	if h.Credential != nil {
		credential = string(*h.Credential)
	}
	status := int64(0)
	if h.LastErrorStatus != nil {
		status = *h.LastErrorStatus
	}
	if (h.ErrorPresent != nil && *h.ErrorPresent) || status != 0 || credential == "missing" || credential == "rejected" {
		return diagnostics.Check{Status: "warn", Summary: "Embedding reconciler reports a provider error", Details: doctorHTTPStatus(int(status)), Fix: "Check embedding model, provider availability and credentials on the daemon host; inspect kata daemon logs."}
	}
	if h.Backlog > 0 {
		return diagnostics.Check{Status: "info", Summary: "Embedding reconciler has a pending backlog", Details: []string{fmt.Sprintf("pending: %d", h.Backlog)}}
	}
	return diagnostics.Check{Status: "ok", Summary: "Embedding reconciler reports no provider error or backlog"}
}

func doctorFederation(h *generated.FederationConfigHealth) diagnostics.Check {
	if h == nil || h.Configured == 0 {
		return diagnostics.Check{Status: "info", Summary: "No declarative federation mapping health is reported; replication and hub connectivity are not probed"}
	}
	category := ""
	if h.LastErrorCategory != nil {
		category = *h.LastErrorCategory
	}
	status := int64(0)
	if h.LastErrorStatus != nil {
		status = *h.LastErrorStatus
	}
	if h.Reconciled != h.Configured || h.Conflicted > 0 || h.Pending > 0 || strings.TrimSpace(category) != "" || status != 0 {
		return diagnostics.Check{Status: "warn", Summary: "Declarative federation mappings are pending, conflicted or report an error", Details: []string{fmt.Sprintf("configured: %d; reconciled: %d; pending: %d; conflicted: %d", h.Configured, h.Reconciled, h.Pending, h.Conflicted)}, Fix: "Inspect federation configuration and kata daemon logs on the daemon host; verify mapping and credential origins."}
	}
	return diagnostics.Check{Status: "ok", Summary: "Declarative federation mappings report convergence; replication and hub connectivity are not probed"}
}
