package client

import (
	"context"

	"go.kenn.io/kata/internal/teammate/selection"
)

// WithTeammate selects display contribution for mutations on this context.
// Empty clears an inherited selection. It never changes comment addressing.
func WithTeammate(ctx context.Context, handle string) context.Context {
	return selection.WithRequest(ctx, handle)
}
