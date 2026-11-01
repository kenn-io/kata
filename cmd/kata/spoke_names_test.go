package main

import (
	"encoding/json"
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
