package main

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
)

// A CLI caller can retain a fresh request identity without a reachable daemon.
// Generation must not submit a request itself.
func TestCronNewUIDOffline(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	out, _, err := executeCronCLI(t, "http://127.0.0.1:1", nil, "new-uid")
	require.NoError(t, err)
	var result struct {
		UID string `json:"uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.True(t, uid.Valid(result.UID), out)
	require.Equal(t, strings.ToUpper(result.UID), result.UID)
}
