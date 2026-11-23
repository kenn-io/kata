package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2E_LegacyStartSupersedesNativeSession(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "native session")
	resetFlags(t)
	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(t.Context(), env.URL))
	flags.Workspace = dir
	d := &liveAttnDaemon{cmd: cmd}
	require.NoError(t, attnStartSession(d, ref, "old-session"))
	attnStart(d, ref)
	require.NoError(t, attnEndSession(d, ref, "old-session"))
	value, ok := attnMetaValue(t, env, pid, ref, attentionKey)
	require.True(t, ok)
	assert.Equal(t, attnValueOK, value)
}
