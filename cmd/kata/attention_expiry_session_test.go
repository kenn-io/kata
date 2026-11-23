package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestE2E_LegacyStartSupersedesExpiredNativeSession(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "expired session")
	resetFlags(t)
	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(t.Context(), env.URL))
	flags.Workspace = dir
	d := &liveAttnDaemon{cmd: cmd}
	require.NoError(t, attnStartSession(d, ref, "old-session"))
	runCLIAs(t, env, dir, "worker", "claim", ref, "--ttl", "1m")
	_, err := env.DB.ExpireAssignments(t.Context(), db.ExpireAssignmentsParams{ProjectID: pid, Now: time.Now().Add(2 * time.Minute)})
	require.NoError(t, err)
	runCLIAs(t, env, dir, "next-worker", "claim", ref, "--ttl", "1m")
	attnStart(d, ref)
	require.NoError(t, attnEndSession(d, ref, "old-session"))
	value, ok := attnMetaValue(t, env, pid, ref, attentionKey)
	require.True(t, ok)
	assert.Equal(t, attnValueOK, value)
	_, ok = attnMetaValue(t, env, pid, ref, attentionMsgKey)
	assert.False(t, ok)
}
