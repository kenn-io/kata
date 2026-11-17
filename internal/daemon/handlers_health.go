package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/version"
)

// registerHealthHandlers installs /api/v1/ping and /api/v1/health on humaAPI.
// /ping is the cheap liveness probe (no DB touch). /health reads
// meta.schema_version and reports uptime relative to cfg.StartedAt.
func registerHealthHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "ping",
		Method:      "GET",
		Path:        "/api/v1/ping",
	}, func(ctx context.Context, _ *struct{}) (*api.PingResponse, error) {
		_ = ctx
		out := &api.PingResponse{}
		out.Body.OK = true
		out.Body.Service = "kata"
		out.Body.Version = version.Version
		out.Body.PID = os.Getpid()
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "health",
		Method:      "GET",
		Path:        "/api/v1/health",
	}, func(ctx context.Context, _ *struct{}) (*api.HealthResponse, error) {
		schema, err := cfg.DB.SchemaVersion(ctx)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.HealthResponse{}
		out.Body.OK = true
		out.Body.SchemaVersion = schema
		out.Body.APISchemaVersion = APISchemaVersion
		out.Body.Version = version.Version
		out.Body.StartedAt = cfg.StartedAt
		out.Body.Uptime = time.Since(cfg.StartedAt).Round(time.Second).String()
		diagnostics := projectDiagnosticOwnerAuthority(ctx)
		if diagnostics {
			out.Body.DBPath = cfg.DB.Path()
		}
		if diagnostics && cfg.ReconcilerHealth != nil {
			h := cfg.ReconcilerHealth()
			out.Body.Embeddings = &api.EmbeddingsHealth{
				Configured:      h.Configured,
				ErrorPresent:    h.LastError != "",
				LastSuccessAt:   h.LastSuccessAt,
				LastErrorStatus: h.LastErrorStatus,
				Embedded:        h.Embedded,
				Skipped:         h.Skipped,
				Backlog:         h.Backlog,
				RatePerSecond:   h.RatePerSecond,
				ETASeconds:      h.ETASeconds,
				StartedAt:       h.StartedAt,
				LastProgressAt:  h.LastProgressAt,
			}
			if cfg.Embedder != nil {
				c := cfg.Embedder.CredentialHealth()
				out.Body.Embeddings.Credential = c.Credential
				out.Body.Embeddings.CredentialSource = c.CredentialSource
				out.Body.Embeddings.CredentialReason = c.CredentialReason
				// Credential and built-in setup errors exclude provider-reflected issue text.
				out.Body.Embeddings.LastError = c.LastError
				out.Body.Embeddings.LastErrorAt = c.LastErrorAt
				if c.LastErrorStatus != 0 || h.LastErrorStatus == 401 || h.LastErrorStatus == 403 {
					out.Body.Embeddings.LastErrorStatus = c.LastErrorStatus
				}
			}
		}
		if diagnostics && cfg.Docbank != nil {
			if checked, err := cfg.Docbank.CheckKey(); checked {
				out.Body.Docbank = docbankHealth(err)
			}
		}
		if diagnostics && cfg.FederationConfigHealth != nil {
			health := cfg.FederationConfigHealth()
			out.Body.FederationConfig = &health
		}
		if diagnostics && cfg.RootSigning != nil {
			out.Body.RootSigning, err = cfg.RootSigning.Health(ctx, cfg.DB)
			if err != nil {
				return nil, internalAPIError(err)
			}
		}
		if cfg.IdleShutdownHealth != nil {
			snapshot := cfg.IdleShutdownHealth()
			if snapshot.Timeout > 0 && snapshot.State != IdleStateDisabled {
				idle := api.IdleShutdownHealth{
					Timeout: snapshot.Timeout.String(),
					State:   string(snapshot.State),
				}
				if !snapshot.Deadline.IsZero() {
					deadline := snapshot.Deadline
					idle.Deadline = &deadline
				}
				out.Body.IdleShutdown = &idle
			}
		}
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "probeDocbank",
		Method:      "GET",
		Path:        "/api/v1/docbank/probe",
		Summary:     "Check that the daemon reaches Docbank and Docbank accepts its key",
		Errors:      []int{404, 503},
	}, func(ctx context.Context, _ *struct{}) (*api.DocbankProbeResponse, error) {
		// Same callers and same configuration as health's docbank block.
		var checked bool
		var err error
		if projectDiagnosticOwnerAuthority(ctx) && cfg.Docbank != nil {
			checked, err = cfg.Docbank.CheckKey()
		}
		if !checked {
			return nil, api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
		}
		if err != nil {
			return nil, docbankKeyUnavailable(err)
		}
		return &api.DocbankProbeResponse{Body: probeDocbank(ctx, cfg.Docbank)}, nil
	})
}

func docbankHealth(err error) *api.DocbankHealth {
	if err == nil {
		return &api.DocbankHealth{Credential: "ok"}
	}
	h := &api.DocbankHealth{Credential: "missing", CredentialReason: err.Error()}
	if e, ok := errors.AsType[*config.CredentialSourceError](err); ok {
		h.CredentialSource, h.CredentialReason = e.Source, e.Reason
	}
	return h
}

const docbankProbeTimeout = 5 * time.Second

// probeDocbank reads Docbank's root folder. Only a 2xx answer shows that the
// address is Docbank and that it accepts the key: a 404, or a 2xx whose body
// is not a folder, could come from any server at a wrong URL.
func probeDocbank(ctx context.Context, c *docbank.Client) api.DocbankProbe {
	ctx, cancel := context.WithTimeout(ctx, docbankProbeTimeout)
	defer cancel()
	root, err := c.ResolvePath(ctx, "/")
	if err == nil {
		return api.DocbankProbe{Reachable: true, KeyAccepted: root.ID > 0 && root.Kind == "dir"}
	}
	if e, ok := errors.AsType[*docbank.Error](err); ok && (e.Status != 0 || e.Malformed) {
		return api.DocbankProbe{Reachable: true, HTTPStatus: e.Status}
	}
	return api.DocbankProbe{}
}
