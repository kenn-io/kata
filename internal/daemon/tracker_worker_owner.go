package daemon

import (
	"context"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

// authorizeWorkerOwner verifies a retained root pin, live project enrollment,
// current owner incarnation, owner-local actor map and exact connection grant
// before any credential resolution. A root actor remains root attribution only.
func (r *TrackerRuntime) authorizeWorkerOwner(ctx context.Context, signed tracker.SignedWorkerTicket, args []byte) (context.Context, trackerLocalConnection, error) {
	empty := trackerLocalConnection{}
	t := signed.Ticket
	if len(args) > 48<<10 || t.ArgumentsDigest != tracker.WorkerArgumentsDigest(args) || t.Selection.OwnerInstanceUID != r.store.InstanceUID() {
		return ctx, empty, tracker.ErrInvalid
	}
	if err := r.verifyWorkerTransport(ctx, signed, args); err != nil {
		return ctx, empty, err
	}
	snapshot, err := r.current()
	if err != nil || snapshot.incarnation != t.OwnerEpoch {
		return ctx, empty, tracker.ErrUnavailable
	}
	c, ok := snapshot.connections[t.Selection.ConnectionUID]
	if !ok || c.descriptor.Selection != t.Selection || c.descriptor.Provider != t.Provider {
		return ctx, empty, tracker.ErrUnavailable
	}
	ctx, err = r.mappedContext(ctx, snapshot, t.ProjectUID, t.Principal)
	if err != nil {
		return ctx, empty, err
	}
	if !c.descriptor.Allows(t.RootInstanceUID, t.ProjectUID, t.Principal, t.Action, t.Resource) && (t.SourceKey == "" || !c.descriptor.Allows(t.RootInstanceUID, t.ProjectUID, t.Principal, t.Action, t.SourceKey)) {
		return ctx, empty, tracker.ErrDenied
	}
	source := t.SourceKey
	if source == "" {
		source = t.Resource
	}
	authority := trackerRunAuthority{root: t.RootInstanceUID, project: t.ProjectUID, source: source, resource: t.Resource, principal: t.Principal, action: t.Action, incarnation: snapshot.incarnation}
	ctx = context.WithValue(ctx, trackerRunAuthorityKey{}, authority)
	return tracker.WithRunSelection(ctx, t.Selection), c, nil
}
func workerOwnerAuthorization(c trackerLocalConnection, t tracker.WorkerTicket) trackerWorkerAuthorization {
	result := trackerWorkerAuthorization{Allowed: true, Connection: tracker.PublicDescriptor{Selection: c.descriptor.Selection, Provider: c.descriptor.Provider, Label: c.descriptor.Label, Authority: c.descriptor.Authority}}
	if t.Action == tracker.ActionDiscover {
		public, ok := c.descriptor.Public(t.RootInstanceUID, t.ProjectUID, t.Principal)
		if !ok {
			return trackerWorkerAuthorization{}
		}
		result.Connection = public
	}
	return result
}

// Verify transport/root provenance independently of provider grants so denied
// candidates receive a bounded classified reply and metadata recovery can keep
// working after the provider policy is removed.
func (r *TrackerRuntime) verifyWorkerTransport(ctx context.Context, signed tracker.SignedWorkerTicket, args []byte) error {
	t := signed.Ticket
	if len(args) > 48<<10 || t.ArgumentsDigest != tracker.WorkerArgumentsDigest(args) || t.Selection.OwnerInstanceUID != r.store.InstanceUID() {
		return tracker.ErrInvalid
	}
	purpose := tracker.WorkerExecute
	if t.Purpose == tracker.WorkerAcknowledge && t.Method == t.Provider+".acknowledge" {
		purpose = tracker.WorkerAcknowledge
	}
	pin, err := r.store.RootAuthority(ctx, t.ProjectUID)
	if err != nil || pin.AuthorityUID != t.RootInstanceUID || pin.ProjectUID != t.ProjectUID || signed.Verify(pin.PublicKey, pin.AuthorityUID, pin.ProjectUID, pin.KeyID, purpose, time.Now()) != nil {
		return tracker.ErrDenied
	}
	project, err := r.store.ProjectByUID(ctx, t.ProjectUID)
	if err != nil {
		return tracker.ErrDenied
	}
	binding, err := r.store.FederationBindingByProject(ctx, project.ID)
	if err != nil || !binding.Enabled || binding.Role != db.FederationRoleSpoke || binding.RelayConfig == nil || binding.RelayConfig.UpstreamRevoked || binding.RelayConfig.AuthorityUID != pin.AuthorityUID {
		return tracker.ErrUnavailable
	}
	return nil
}
