package mcpserver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPObservesAdmittedCalls(t *testing.T) {
	var calls atomic.Int64
	session := connectRawTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version",
		ObserveToolCall: func() { calls.Add(1) },
	})
	_, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, calls.Load())
	for _, name := range []string{"kata.load_issue_discovery", "kata.missing", "kata.show"} {
		_, _ = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
	}
	require.EqualValues(t, 3, calls.Load())
}

func TestMCPObservationRequiresAdmission(t *testing.T) {
	var calls int
	concurrent := make(chan struct{}, 1)
	limiter := rate.NewLimiter(1, 1)
	toolError := errors.New("tool failed")
	handler := toolAdmissionMiddleware(limiter, concurrent, func() { calls++ })(
		func(context.Context, string, sdkmcp.Request) (sdkmcp.Result, error) { return nil, toolError },
	)
	_, err := handler(t.Context(), "tools/call", nil)
	require.ErrorIs(t, err, toolError)
	require.Equal(t, 1, calls)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err = handler(ctx, "tools/call", nil)
	require.ErrorContains(t, err, "deadline")
	concurrent <- struct{}{}
	_, err = handler(ctx, "tools/call", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, calls)
}
