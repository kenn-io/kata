package main

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
)

// A CLI caller can retain a fresh request identity without a daemon, workspace,
// project, or external UID tool. Generation must not submit a request itself.
func TestCronNewUIDOffline(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	previous := ""
	for range 2 {
		resetFlags(t)
		cmd := newRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetContext(contextWithBaseURL(t.Context(), "http://127.0.0.1:1"))
		cmd.SetArgs([]string{"--json", "cron", "new-uid"})
		start := time.Now().UTC().Add(-time.Second)
		require.NoError(t, cmd.Execute(), out.String())
		var result struct {
			UID string `json:"uid"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &result))
		require.True(t, uid.Valid(result.UID))
		require.Equal(t, strings.ToUpper(result.UID), result.UID)
		require.NotEqual(t, previous, result.UID)
		at := uid.MustTime(result.UID)
		require.False(t, at.Before(start))
		require.False(t, at.After(time.Now().UTC().Add(time.Second)))
		previous = result.UID
	}
}
