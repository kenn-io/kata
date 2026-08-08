package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	apiVersionReadyAndSearchFilters = "0.8.0"
	apiVersionGlobalListFilters     = "0.9.0"
)

// requireDaemonAPIVersion fails closed before a request that uses query
// parameters older daemons silently ignored. The health endpoint predates the
// filtered global queries, so it is safe to use as the capability handshake.
func requireDaemonAPIVersion(
	ctx context.Context,
	client *http.Client,
	baseURL, required, feature string,
) error {
	status, body, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/health", nil)
	if err != nil {
		return err
	}
	if status >= http.StatusBadRequest {
		return apiErrFromBody(status, body)
	}
	var health struct {
		APISchemaVersion string `json:"api_schema_version"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return fmt.Errorf("decode daemon API version: %w", err)
	}
	reported := strings.TrimSpace(health.APISchemaVersion)
	compatible, valid := apiVersionAtLeast(reported, required)
	if valid && compatible {
		return nil
	}
	if reported == "" {
		reported = "no api_schema_version"
	}
	return &cliError{
		Message: fmt.Sprintf(
			"%s requires daemon API %s or newer; this daemon reports %s; upgrade the daemon",
			feature, required, reported),
		Kind:     kindValidation,
		Code:     "daemon_api_too_old",
		ExitCode: ExitValidation,
	}
}

func apiVersionAtLeast(reported, required string) (atLeast, valid bool) {
	reportedParts, ok := parseAPIVersion(reported)
	if !ok {
		return false, false
	}
	requiredParts, ok := parseAPIVersion(required)
	if !ok {
		return false, false
	}
	for i := range reportedParts {
		if reportedParts[i] != requiredParts[i] {
			return reportedParts[i] > requiredParts[i], true
		}
	}
	return true, true
}

func parseAPIVersion(value string) ([3]int, bool) {
	var out [3]int
	value = strings.TrimSpace(value)
	if suffix := strings.IndexAny(value, "-+"); suffix >= 0 {
		value = value[:suffix]
	}
	parts := strings.Split(value, ".")
	if len(parts) != len(out) {
		return out, false
	}
	for i, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return out, false
		}
		out[i] = parsed
	}
	return out, true
}
