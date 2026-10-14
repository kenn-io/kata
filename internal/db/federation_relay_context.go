package db

import (
	"context"
	"slices"
)

type relayForwardPathKey struct{}

// WithRelayForwardPath carries an already validated ingress path only within
// the storage transaction. It is routing state, never an authorization grant.
func WithRelayForwardPath(ctx context.Context, path []string) context.Context {
	return context.WithValue(ctx, relayForwardPathKey{}, slices.Clone(path))
}

// RelayForwardPath returns a copy of the validated ingress routing path.
func RelayForwardPath(ctx context.Context) []string {
	path, _ := ctx.Value(relayForwardPathKey{}).([]string)
	return slices.Clone(path)
}

// Checkpoint capture and owner replay retain existing durable state without
// creating new ordinary deliveries. Only native snapshot/replay paths set this;
// no request field or transported actor label can establish it.
type relayStateCaptureKey struct{}

// WithRelayStateCapture marks native checkpoint or owner replay without new delivery.
func WithRelayStateCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, relayStateCaptureKey{}, true)
}

// RelayStateCapture reports the native checkpoint or owner replay marker.
func RelayStateCapture(ctx context.Context) bool {
	capture, _ := ctx.Value(relayStateCaptureKey{}).(bool)
	return capture
}

type relayArtifactSourceKey struct{}

// WithRelayArtifactSourceUID retains a validated incoming artifact offer's
// identity during forwarding. This is routing state, never project authority.
func WithRelayArtifactSourceUID(ctx context.Context, sourceUID string) context.Context {
	return context.WithValue(ctx, relayArtifactSourceKey{}, sourceUID)
}

// RelayArtifactSourceUID returns the validated offer identity being forwarded.
func RelayArtifactSourceUID(ctx context.Context) string {
	sourceUID, _ := ctx.Value(relayArtifactSourceKey{}).(string)
	return sourceUID
}
