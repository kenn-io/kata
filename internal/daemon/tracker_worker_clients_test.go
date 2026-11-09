package daemon

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/twentysync"
	"testing"
)

// All remote read/session interfaces are represented explicitly. Constructors
// perform no provider I/O and cannot fall back to owner-local/default clients.
func TestTrackerRemoteClientsKeepSevenTypedProviderInterfaces(t *testing.T) {
	run := &trackerRemoteRun{}
	clients := newTrackerRemoteClients(run)
	cases := []func(context.Context) error{
		func(ctx context.Context) error {
			_, err := clients.github.(githubsync.BindingSessionFetcher).ForBinding(ctx, githubsync.Binding{})
			return err
		},
		func(ctx context.Context) error { _, err := clients.notion.ForRun(ctx); return err },
		func(ctx context.Context) error { _, err := clients.linear.ForRun(ctx, linearsync.Config{}); return err },
		func(ctx context.Context) error { _, err := clients.plane.ForRun(ctx, planesync.Config{}); return err },
		func(ctx context.Context) error {
			_, err := clients.ticktick.ForRun(ctx, tickticksync.Config{})
			return err
		},
		func(ctx context.Context) error {
			_, err := clients.todoist.ForRun(ctx, todoistsync.Config{})
			return err
		},
		func(ctx context.Context) error { _, err := clients.twenty.ForRun(ctx, twentysync.Config{}); return err },
	}
	for _, call := range cases {
		require.NoError(t, call(t.Context()))
	}
	// Keep a compile-time interface contract for the source-only validation path.
	var _ notionsync.Session = (*trackerRemoteNotion)(nil)
}

func TestRemoteWriteNeedsOriginalNativeStatusInvocation(t *testing.T) {
	run := &trackerRemoteRun{}
	admitted := 0
	_, err := run.writeStatus(t.Context(), trackerWorkerArguments{ID: "item", Target: "closed"}, func() error { admitted++; return nil })
	require.Error(t, err)
	require.Zero(t, admitted, "a generic selected session cannot invent a root event or invoke delivery")
}
