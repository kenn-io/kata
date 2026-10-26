package client

import (
	"context"

	"go.kenn.io/kata/internal/teammate"
)

// WithTeammate selects display contribution for mutations on this context.
// Empty clears an inherited selection. It never changes comment addressing.
func WithTeammate(ctx context.Context, handle string) context.Context {
	return teammate.WithRequest(ctx, handle)
}
