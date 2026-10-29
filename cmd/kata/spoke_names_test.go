package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFederationSpokeNamesCLI(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	runCLI(t, env, dir, "federation", "enable")
	uid := env.DB.InstanceUID()
	runCLI(t, env, dir, "federation", "enroll", "--spoke-instance", uid, "--spoke-name", "laptop", "--hub-url", env.URL, "--actor", "member")
	require.Contains(t, runCLI(t, env, dir, "federation", "enrollments", "list"), uid+" (laptop)")
	runCLI(t, env, dir, "federation", "enrollments", "name", uid, "build-box")
	out := runCLI(t, env, dir, "--json", "federation", "enrollments", "list")
	var list struct {
		Enrollments []map[string]any `json:"enrollments"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &list))
	require.Equal(t, "build-box", list.Enrollments[0]["spoke_name"])
	runCLI(t, env, dir, "federation", "enrollments", "name", uid, "--clear")
	require.NotContains(t, runCLI(t, env, dir, "federation", "enrollments", "list"), "build-box")
}

// An omitted optional flag must keep enrollment working with older hubs.
func TestFederationSpokeNameOmittedCLI(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/api/v1/federation/enrollments" {
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			_ = req.Body.Close()
			req.Body = io.NopCloser(strings.NewReader(string(raw)))
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				return nil, err
			}
			if _, present := body["spoke_name"]; present {
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":400,"error":{"code":"validation","message":"unknown spoke_name"}}`))}, nil
			}
		}
		return original.RoundTrip(req)
	})
	runCLI(t, env, dir, "federation", "enroll", "--spoke-instance", env.DB.InstanceUID(), "--hub-url", env.URL, "--capabilities", "pull", "--actor", "member")
}
