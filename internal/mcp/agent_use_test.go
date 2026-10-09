package mcpserver

import (
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPObservesAdmittedCalls(t *testing.T) {
	var calls atomic.Int64
	session := connectRawTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version",
		ObserveToolCall: func() { calls.Add(1) },
	})
	_, _ = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_issue_discovery", Arguments: map[string]any{}})
	require.EqualValues(t, 1, calls.Load())
}
