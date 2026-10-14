package db

import (
	"context"
	"slices"
)

type rootAttributionContextKey struct{}
type rootAttributionContext struct {
	signer RootAttributionSigner
	actor  string
	owner  bool
}

// WithRootAttribution carries an already authenticated write account and local
// signing material. It is an internal request context, never a transport field.
// Source author/agent labels cannot resolve or replace this account.
func WithRootAttribution(ctx context.Context, signer RootAttributionSigner, authenticatedActor string) context.Context {
	signer.PrivateKey = slices.Clone(signer.PrivateKey)
	return context.WithValue(ctx, rootAttributionContextKey{}, rootAttributionContext{signer: signer, actor: authenticatedActor})
}

// WithOwnerRootAttribution retains the existing request-actor convention only
// after a real daemon-owner transport or owner-local session has been admitted.
// Network principals with a bound actor must use WithRootAttribution instead.
func WithOwnerRootAttribution(ctx context.Context, signer RootAttributionSigner) context.Context {
	signer.PrivateKey = slices.Clone(signer.PrivateKey)
	return context.WithValue(ctx, rootAttributionContextKey{}, rootAttributionContext{signer: signer, owner: true})
}

// RootAttributionOwner reports whether the request carries admitted daemon-owner authority.
func RootAttributionOwner(ctx context.Context) bool {
	value, _ := ctx.Value(rootAttributionContextKey{}).(rootAttributionContext)
	return value.owner
}

// RootAttributionFromContext returns the authenticated account and local signing material.
func RootAttributionFromContext(ctx context.Context) (RootAttributionSigner, string, bool) {
	value, ok := ctx.Value(rootAttributionContextKey{}).(rootAttributionContext)
	return value.signer, value.actor, ok
}

// ApplyRootAttributionFence locks the policy epoch before domain/event writes,
// preserving lock order with policy administration. The exact project is checked
// again when the event is signed, inside this same transaction.
func ApplyRootAttributionFence(ctx context.Context, store ProjectAccessStorage, tx Transaction) error {
	_, actor, ok := RootAttributionFromContext(ctx)
	if !ok {
		return nil
	}
	if RootAttributionOwner(ctx) {
		// An empty target list takes the policy epoch lock without imposing team
		// membership on the owner. It does not remove any other transaction fence.
		ctx = WithUnrestrictedProjectWrites(ctx)
	}
	return store.ProjectAccessTransactionFence(actor, nil)(ctx, tx)
}

// RemoteEventFromStored preserves source identity when adding separate receipts
// or forwarding an event. It omits only backend-local IDs and display short IDs.
func RemoteEventFromStored(event Event) RemoteEvent {
	return RemoteEvent{
		EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter, ContentHash: event.ContentHash,
		Payload: []byte(event.Payload), CreatedAt: event.CreatedAt,
	}
}
