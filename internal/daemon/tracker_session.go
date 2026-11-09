package daemon

import (
	"context"

	"go.kenn.io/kata/internal/tracker"
)

// trackerSessionGuard captures authority at session creation. Method callers
// supply only lifetime/cancellation; they cannot substitute another account or
// principal in an already opened session. Every method checks the live snapshot.
type trackerSessionGuard struct {
	runtime   *TrackerRuntime
	selection tracker.Selection
	authority trackerRunAuthority
	provider  string
}

func (r *TrackerRuntime) sessionGuard(ctx context.Context, provider string) (trackerSessionGuard, error) {
	if _, err := r.selectedMethodConnection(ctx, provider, tracker.ActionRead); err != nil {
		return trackerSessionGuard{}, err
	}
	selection, _ := tracker.RunSelection(ctx)
	authority := ctx.Value(trackerRunAuthorityKey{}).(trackerRunAuthority)
	return trackerSessionGuard{runtime: r, selection: selection, authority: authority, provider: provider}, nil
}
func (g trackerSessionGuard) authorize(ctx context.Context) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	ctx = tracker.WithRunSelection(ctx, g.selection)
	ctx = context.WithValue(ctx, trackerRunAuthorityKey{}, g.authority)
	_, err := g.runtime.selectedMethodConnection(ctx, g.provider, tracker.ActionRead)
	return ctx, err
}

// Provider mutation sessions stay closed until the root's durable status
// coordinator supplies a current attempt. Merely retaining a credential-bearing
// read session or a service grant is insufficient to dispatch a write.
func (g trackerSessionGuard) writeContext(ctx context.Context, target string) (context.Context, error) {
	ctx, err := g.authorize(ctx)
	if err != nil {
		return ctx, err
	}
	if _, err := g.runtime.selectedMethodConnection(ctx, g.provider, tracker.ActionStatus); err != nil {
		return ctx, err
	}
	attempt, ok := ctx.Value(trackerStatusAttemptKey{}).(trackerStatusAttempt)
	if !ok || attempt.runtime != g.runtime || attempt.source != g.authority.source || attempt.target != target || g.authority.principal.Kind != tracker.SubjectService {
		return ctx, tracker.ErrUnavailable
	}
	return ctx, nil
}

func (g trackerSessionGuard) admission(ctx context.Context, target string, admit func() error) func() error {
	return func() error {
		if _, err := g.writeContext(ctx, target); err != nil {
			return err
		}
		if admit == nil {
			return tracker.ErrUnavailable
		}
		return admit()
	}
}
