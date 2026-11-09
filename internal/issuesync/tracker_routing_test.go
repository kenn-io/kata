package issuesync

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"testing"
	"time"
)

// Contract: every named binding is routed before status or content code can
// resolve a legacy provider credential, including scheduled runs.
func TestNamedBindingRequiresRoutingBeforeAdapter(t *testing.T) {
	for _, provider := range []string{"github", "notion", "linear", "plane", "ticktick", "todoist", "twenty"} {
		t.Run(provider, func(t *testing.T) {
			store := testStore(t)
			project, err := store.CreateProject(t.Context(), "named-project")
			require.NoError(t, err)
			selection := db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
			binding, err := store.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: provider, SourceKey: provider + ":source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300, Connection: &selection})
			require.NoError(t, err)
			calls := 0
			adapter := &namedRoutingAdapter{provider: provider, testAdapter: &testAdapter{prepare: func(context.Context, db.IssueSyncBinding, time.Time) (Prepared, error) {
				calls++
				return Prepared{}, nil
			}}}
			runner := NewRunner(RunnerConfig{Store: store, Adapter: adapter})
			_, err = runner.RunOnce(t.Context(), binding.ID)
			require.ErrorIs(t, err, tracker.ErrUnavailable)
			require.Zero(t, calls)
		})
	}
}

type namedRoutingAdapter struct {
	provider string
	*testAdapter
}

func (a *namedRoutingAdapter) Provider() string { return a.provider }
