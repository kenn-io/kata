package daemon

import (
	"context"
	"errors"
	"testing"

	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/twentysync"
)

// Contract: selected context never reaches a legacy fetcher, even when the
// runtime/selected connection is missing. Nil legacy seams would panic if used.
func TestTrackerFetcherMissingNamedAuthorityNeverFallsBack(t *testing.T) {
	selection := tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
	ctx := tracker.WithRunSelection(t.Context(), selection)
	cases := map[string]func(context.Context) error{
		"github": func(ctx context.Context) error {
			_, err := (trackerGitHubFetcher{}).ForBinding(ctx, githubsync.Binding{})
			return err
		},
		"notion": func(ctx context.Context) error { _, err := (trackerNotionFetcher{}).ForRun(ctx); return err },
		"linear": func(ctx context.Context) error {
			_, err := (trackerLinearFetcher{}).ForRun(ctx, linearsync.Config{})
			return err
		},
		"plane": func(ctx context.Context) error {
			_, err := (trackerPlaneFetcher{}).ForRun(ctx, planesync.Config{})
			return err
		},
		"ticktick": func(ctx context.Context) error {
			_, err := (trackerTickTickFetcher{}).ForRun(ctx, tickticksync.Config{})
			return err
		},
		"todoist": func(ctx context.Context) error {
			_, err := (trackerTodoistFetcher{}).ForRun(ctx, todoistsync.Config{})
			return err
		},
		"twenty": func(ctx context.Context) error {
			_, err := (trackerTwentyFetcher{}).ForRun(ctx, twentysync.Config{})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			for _, selectedCtx := range []context.Context{ctx, tracker.WithRunSelection(t.Context(), tracker.Selection{})} {
				if err := call(selectedCtx); !errors.Is(err, tracker.ErrUnavailable) {
					t.Fatal("named path did not fail unavailable", err)
				}
			}
		})
	}
}
