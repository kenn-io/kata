package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/twentysync"
)

type trackerTestRunner interface {
	RunOnce(context.Context, int64) (issuesync.RunResult, error)
}
type trackerTestRoute func(context.Context, db.IssueSyncBinding) (context.Context, error)

func TestEveryProviderRunnerPropagatesNamedRouting(t *testing.T) {
	factories := map[string]func(db.Storage, trackerTestRoute) trackerTestRunner{
		"github": func(s db.Storage, r trackerTestRoute) trackerTestRunner {
			return githubsync.NewRunner(githubsync.RunnerConfig{Store: s, Fetcher: trackerGitHubFetcher{}, RouteBinding: r})
		},
		"notion": func(s db.Storage, r trackerTestRoute) trackerTestRunner {
			return notionsync.NewRunner(notionsync.RunnerConfig{Store: s, Fetcher: trackerNotionFetcher{}, RouteBinding: r})
		},
		"linear": func(s db.Storage, r trackerTestRoute) trackerTestRunner {
			return linearsync.NewRunner(linearsync.RunnerConfig{Store: s, Fetcher: trackerLinearFetcher{}, RouteBinding: r})
		},
		"plane": func(s db.Storage, r trackerTestRoute) trackerTestRunner {
			return planesync.NewRunner(planesync.RunnerConfig{Store: s, Fetcher: trackerPlaneFetcher{}, RouteBinding: r})
		},
		"ticktick": func(s db.Storage, r trackerTestRoute) trackerTestRunner {
			return tickticksync.NewRunner(tickticksync.RunnerConfig{Store: s, Fetcher: trackerTickTickFetcher{}, RouteBinding: r})
		},
		"todoist": func(s db.Storage, r trackerTestRoute) trackerTestRunner {
			return todoistsync.NewRunner(todoistsync.RunnerConfig{Store: s, Fetcher: trackerTodoistFetcher{}, RouteBinding: r})
		},
		"twenty": func(s db.Storage, r trackerTestRoute) trackerTestRunner {
			return twentysync.NewRunner(twentysync.RunnerConfig{Store: s, Fetcher: trackerTwentyFetcher{}, RouteBinding: r})
		},
	}
	for provider, factory := range factories {
		t.Run(provider, func(t *testing.T) {
			ctx := t.Context()
			store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			project, err := store.CreateProject(ctx, "tracker-project")
			require.NoError(t, err)
			selection := tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
			binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: provider, SourceKey: provider + ":source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300, Connection: &selection})
			require.NoError(t, err)
			calls := 0
			runner := factory(store, func(ctx context.Context, b db.IssueSyncBinding) (context.Context, error) {
				calls++
				require.Equal(t, binding.ID, b.ID)
				return ctx, tracker.ErrDenied
			})
			_, err = runner.RunOnce(ctx, binding.ID)
			require.ErrorIs(t, err, tracker.ErrDenied)
			require.Equal(t, 1, calls)
		})
	}
}
