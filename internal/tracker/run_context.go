package tracker

import "context"

type runSelectionKey struct{}

// WithRunSelection is used only by an admitted in-process binding router. It
// carries identity, never credentials, and does not itself confer a grant.
func WithRunSelection(ctx context.Context, selection Selection) context.Context {
	return context.WithValue(ctx, runSelectionKey{}, selection)
}

// RunSelection reports presence separately from validity. A malformed present
// selection must fail closed, never become an absent legacy-account choice.
func RunSelection(ctx context.Context) (Selection, bool) {
	selection, ok := ctx.Value(runSelectionKey{}).(Selection)
	return selection, ok
}
