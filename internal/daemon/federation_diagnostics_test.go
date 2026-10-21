package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type federationDiagnosticStore struct {
	db.Storage
	failure error
}

func (s federationDiagnosticStore) EnableProjectFederation(context.Context, int64, string) (db.FederationBinding, error) {
	return db.FederationBinding{}, s.failure
}

// Diagnostics classify failures without copying request content or arbitrary
// database/upstream errors. Removing the API boundary logger breaks this test.
func TestFederationFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name               string
		failure            error
		status             int
		code, level, cause string
	}{
		{"expected", db.ErrNotFound, 404, "federation_not_found", "DEBUG", "not_found"},
		{"validation", nil, 400, "validation", "DEBUG", "request_rejected"},
		{"unexpected", errors.New("SELECT token FROM secrets WHERE value='sql-secret'"), 500, "internal", "WARN", "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			env := testenv.New(t, func(cfg *daemon.ServerConfig) {
				cfg.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
				cfg.DB = federationDiagnosticStore{Storage: cfg.DB, failure: tc.failure}
			})
			project, err := env.DB.CreateProject(t.Context(), "hub-project")
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/federation/enable?token=query-secret", project.ID)
			requestBody := map[string]any{"actor": "body-secret"}
			if tc.status == 400 {
				requestBody["actor"] = map[string]any{"token": "body-secret"}
			}
			resp, body := envDoRaw(t, env, http.MethodPost, path, requestBody,
				map[string]string{"X-Source-Marker": "header-marker"})
			require.Equal(t, tc.status, resp.StatusCode, string(body))
			var envelope struct{ Error struct{ Code string } }
			require.NoError(t, json.Unmarshal(body, &envelope))
			require.Equal(t, tc.code, envelope.Error.Code)
			if tc.status == 404 {
				assert.JSONEq(t, `{"status":404,"error":{"code":"federation_not_found","message":"federation metadata not found"}}`, string(body))
			}
			if tc.status == 500 {
				assert.JSONEq(t, `{"status":500,"error":{"code":"internal","message":"SELECT token FROM secrets WHERE value='sql-secret'"}}`, string(body))
			}
			require.NotEmpty(t, logs.String(), "one classified failure diagnostic is required")
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			require.Len(t, lines, 1)
			var entry map[string]any
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
			assert.Equal(t, "enableProjectFederation", entry["operation"])
			assert.Equal(t, float64(tc.status), entry["status"])
			assert.Equal(t, tc.code, entry["code"])
			assert.Equal(t, tc.cause, entry["cause"])
			assert.Equal(t, tc.level, entry["level"])
			assert.Len(t, entry, 7, "only standard log fields and bounded diagnostics are allowed")
			for _, marker := range []string{"header-marker", "query-secret", "body-secret", "sql-secret", "SELECT"} {
				assert.NotContains(t, logs.String(), marker)
			}
		})
	}
}

func TestFederationMetadataFailureDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	project, err := env.DB.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	resp, body := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation", project.ID), nil, nil)
	require.Equal(t, 404, resp.StatusCode, string(body))
	require.NotEmpty(t, logs.String())
	var entry map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
	assert.Equal(t, "getProjectFederation", entry["operation"])
	assert.Equal(t, "federation_not_found", entry["code"])
	logs.Reset()
	resp, body = envDoRaw(t, env, http.MethodPost, "/api/v1/projects/resolve", map[string]any{"name": "missing-project"}, nil)
	require.Equal(t, 404, resp.StatusCode, string(body))
	assert.Empty(t, logs.String(), "non-federation failures must not emit federation diagnostics")
}

// Arbitrary upstream messages must never change the bounded diagnostic fields.
// Native fuzzing keeps this property dependency-free in the upstream project.
func FuzzFederationDiagnosticsExcludeUntrustedContent(f *testing.F) {
	for _, seed := range []string{"", "Bearer token-shaped-secret", "SELECT secret FROM accounts", "\nforged warning\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, untrusted string) {
		var logs bytes.Buffer
		env := testenv.New(t, func(cfg *daemon.ServerConfig) {
			cfg.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			cfg.DB = federationDiagnosticStore{Storage: cfg.DB, failure: errors.New(untrusted)}
		})
		project, err := env.DB.CreateProject(t.Context(), "hub-project")
		require.NoError(t, err)
		url := fmt.Sprintf("%s/api/v1/projects/%d/federation/enable", env.URL, project.ID)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(`{"actor":"member"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, _ := env.HTTP.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		// A malformed upstream string may itself fail response serialization;
		// the diagnostic still must contain exactly these safe fields.
		var entry map[string]any
		require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
		delete(entry, "time")
		assert.Equal(t, map[string]any{"level": "WARN", "msg": "federation request failed", "operation": "enableProjectFederation", "status": float64(500), "code": "internal", "cause": "internal"}, entry)
	})
}
