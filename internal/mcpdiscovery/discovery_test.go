package mcpdiscovery

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

// Listener publication, status, and cleanup are the client discovery contract.
func TestPublishedListenerStatusAndCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mcp")
	require.NoError(t, os.Mkdir(dir, 0o700))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	cleanup, err := Publish(dir, listener.Addr().String(), "test-listener-token", "http://127.0.0.1:4321")
	require.NoError(t, err)
	rows, err := List(dir)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "http://"+listener.Addr().String()+"/mcp", rows[0].URL)
	assert.Equal(t, "http://127.0.0.1:4321", rows[0].BackendURL)
	token, err := os.ReadFile(rows[0].TokenPath)
	require.NoError(t, err)
	assert.Equal(t, "test-listener-token", string(token))
	require.NoError(t, cleanup())
	rows, err = List(dir)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestListOmitsReusedPID(t *testing.T) {
	for _, identityAvailable := range []bool{true, false} {
		name := "start time"
		if identityAvailable {
			name = "process identity"
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "mcp")
			cleanup, err := Publish(dir, "127.0.0.1:9876", "test-listener-token", "")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, cleanup()) })
			rows, err := List(dir)
			require.NoError(t, err)
			require.Len(t, rows, 1)

			store := kitdaemon.RuntimeStore{Dir: dir, Prefix: "mcp"}
			records, err := store.List()
			require.NoError(t, err)
			require.Len(t, records, 1)
			record := records[0]
			if identityAvailable {
				identity, ok := kitdaemon.ReadProcessIdentity(record.PID)
				require.True(t, ok)
				encoded := string(identity)
				replacement := byte('0')
				if encoded[len(encoded)-1] == replacement {
					replacement = '1'
				}
				record.ProcessIdentityV2 = kitdaemon.ProcessIdentity(encoded[:len(encoded)-1] + string(replacement))
			} else {
				record.ProcessIdentity = ""
				record.ProcessIdentityV2 = ""
				record.StartedAt = time.Unix(1, 0)
			}
			recordPath, err := store.Write(record)
			require.NoError(t, err)

			rows, err = List(dir)
			require.NoError(t, err)
			assert.Empty(t, rows, "a reused PID must not advertise the endpoint or retained token path")
			assert.FileExists(t, recordPath, "status must remain observational")
			assert.FileExists(t, record.Metadata["token_path"], "status must not prune retained credentials")
		})
	}
}
