package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"sync"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

type trackerWorkerAuthorization struct {
	Allowed    bool                     `json:"allowed"`
	Connection tracker.PublicDescriptor `json:"connection"`
}
type trackerRemoteRun struct {
	metadata           bool
	source             string
	action             tracker.Action
	runtime            *TrackerRuntime
	snapshot           *trackerRuntimeSnapshot
	project            string
	principal          tracker.Principal
	descriptor         tracker.PublicDescriptor
	ownerEpoch         string
	resource           string
	runUID, sessionUID string
	mu                 sync.Mutex
	closed             bool
}

func (r *TrackerRuntime) remoteRun(snapshot *trackerRuntimeSnapshot, project string, p tracker.Principal, selection tracker.Selection, resource string) (*trackerRemoteRun, error) {
	if selection.OwnerInstanceUID == r.store.InstanceUID() || selection.Validate() != nil || !tracker.ValidIdentityText(resource, 2048) {
		return nil, tracker.ErrInvalid
	}
	for _, owner := range r.workerBroker.owners(project, time.Now()) {
		if owner.OwnerInstanceUID != selection.OwnerInstanceUID {
			continue
		}
		for _, d := range owner.Connections {
			if d.Selection == selection {
				runUID, err := uid.New()
				if err != nil {
					return nil, err
				}
				sessionUID, err := uid.New()
				if err != nil {
					return nil, err
				}
				return &trackerRemoteRun{runtime: r, snapshot: snapshot, project: project, principal: p, descriptor: d, ownerEpoch: owner.OwnerEpoch, resource: resource, runUID: runUID, sessionUID: sessionUID}, nil
			}
		}
	}
	return nil, tracker.ErrUnavailable
}
func (r *trackerRemoteRun) ticket(ctx context.Context, method string, action tracker.Action, arguments []byte) (tracker.SignedWorkerTicket, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return tracker.SignedWorkerTicket{}, tracker.ErrUnavailable
	}
	var snapshot *trackerRuntimeSnapshot
	var err error
	if r.metadata && (!workerRecoveryMethod(r.descriptor.Provider+"."+method) || action != tracker.ActionRecover) {
		return tracker.SignedWorkerTicket{}, tracker.ErrDenied
	}
	if !r.metadata {
		snapshot, err = r.runtime.current()
		if err != nil || snapshot.incarnation != r.snapshot.incarnation {
			return tracker.SignedWorkerTicket{}, tracker.ErrUnavailable
		}
		if _, err := r.runtime.mappedContext(ctx, snapshot, r.project, r.principal); err != nil {
			return tracker.SignedWorkerTicket{}, err
		}
	}
	if r.runtime.signing == nil {
		return tracker.SignedWorkerTicket{}, tracker.ErrUnavailable
	}
	pin, err := r.runtime.store.RootAuthority(ctx, r.project)
	if err != nil {
		return tracker.SignedWorkerTicket{}, tracker.ErrUnavailable
	}
	signer := r.runtime.signing.Signer()
	if !db.RootSignerMatches(pin, signer) || pin.AuthorityUID != r.runtime.store.InstanceUID() {
		return tracker.SignedWorkerTicket{}, tracker.ErrUnavailable
	}
	actor := "system"
	rootEpoch := r.runtime.epoch
	if !r.metadata {
		rootEpoch = snapshot.incarnation
		for _, mapping := range snapshot.mappings {
			if mapping.ProjectUID == r.project && mapping.Principal == r.principal {
				actor = mapping.Actor
			}
		}
	}
	callUID, err := uid.New()
	if err != nil {
		return tracker.SignedWorkerTicket{}, err
	}
	t := tracker.WorkerTicket{Version: 1, Purpose: tracker.WorkerExecute, KeyID: pin.KeyID, RootInstanceUID: pin.AuthorityUID, ProjectUID: r.project, Selection: r.descriptor.Selection, Principal: r.principal, RootActor: actor, Action: action, Resource: r.resource, SourceKey: r.source, Provider: r.descriptor.Provider, Method: r.descriptor.Provider + "." + method, RunUID: r.runUID, CallUID: callUID, SessionUID: r.sessionUID, RootEpoch: rootEpoch, OwnerEpoch: r.ownerEpoch, ArgumentsDigest: tracker.WorkerArgumentsDigest(arguments), ExpiresAt: time.Now().Add(30 * time.Second).Unix()}
	if r.metadata && method == "acknowledge" {
		var args trackerWorkerMetadataArgs
		if json.Unmarshal(arguments, &args, json.RejectUnknownMembers(true)) != nil || args.Checkpoint == nil || args.Checkpoint.Validate() != nil || args.Checkpoint.Selection != t.Selection {
			return tracker.SignedWorkerTicket{}, tracker.ErrInvalid
		}
		t.Purpose = tracker.WorkerAcknowledge
		t.WriteUID = args.Checkpoint.WriteUID
		t.ReservationDigest = t.ArgumentsDigest
	}
	return tracker.SignWorkerTicket(t, signer.PrivateKey)
}

// A ticket bounds start admission, not successful delivery. Each call also
// has a finite execution/reply budget independent of the whole import run.
// Expiry cannot renew dispatch authority or erase a retained external hazard.
const trackerWorkerReplyGrace = 5 * time.Minute

func trackerWorkerReplyContext(ctx context.Context, t tracker.WorkerTicket) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, time.Unix(t.ExpiresAt, 0).Add(trackerWorkerReplyGrace))
}

func (r *trackerRemoteRun) invoke(ctx context.Context, method string, action tracker.Action, args, out any, reserved bool) error {
	if r.metadata {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}
	raw, err := json.Marshal(args, json.Deterministic(true))
	if err != nil || len(raw) > 48<<10 {
		return tracker.ErrInvalid
	}
	ticket, err := r.ticket(ctx, method, action, raw)
	if err != nil {
		return err
	}
	ctx, cancel := trackerWorkerReplyContext(ctx, ticket.Ticket)
	defer cancel()
	pending, err := r.runtime.workerBroker.enqueue(ctx, trackerWorkerCommand{Ticket: ticket, Arguments: raw}, reserved)
	if err != nil {
		return err
	}
	defer r.runtime.workerBroker.release(ticket.Ticket.CallUID)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case reply := <-pending.result:
		switch reply.Code {
		case "ok":
			if len(reply.Data) > tracker.WorkerResultBytes || json.Unmarshal(reply.Data, out, json.RejectUnknownMembers(true)) != nil {
				return tracker.ErrUnavailable
			}
			return nil
		default:
			return trackerWorkerError(reply)
		}
	}
}
func (r *trackerRemoteRun) authorize(ctx context.Context, action tracker.Action) (tracker.PublicDescriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var result trackerWorkerAuthorization
	if err := r.invoke(ctx, "authorize", action, struct{}{}, &result, true); err != nil {
		return tracker.PublicDescriptor{}, err
	}
	if !result.Allowed {
		return tracker.PublicDescriptor{}, tracker.ErrDenied
	}
	d := result.Connection
	if d.Selection != r.descriptor.Selection || d.Provider != r.descriptor.Provider || d.Authority.Validate() != nil || !tracker.ValidIdentityText(d.Label, 128) {
		return tracker.PublicDescriptor{}, tracker.ErrUnavailable
	}
	return d, nil
}

// Build only public origin settings for root preflight. All provider I/O uses
// remote proxy clients; the owner never sends selectors/auth modes/credentials.
func trackerRemoteConfig(d tracker.PublicDescriptor) config.TrackerConnectionConfig {
	c := config.TrackerConnectionConfig{UID: d.Selection.ConnectionUID, Generation: d.Selection.Generation, Provider: d.Provider, Label: d.Label}
	switch d.Provider {
	case "github":
		c.GitHub = &config.GitHubSyncConfig{}
	case "notion":
		c.Notion = &config.NotionSyncConfig{}
	case "linear":
		c.Linear = &config.LinearSyncConfig{}
	case "plane":
		c.Plane = &config.PlaneSyncConfig{APIOrigin: d.Authority.APIOrigin, WebOrigin: d.Authority.WebOrigin}
	case "ticktick":
		c.TickTick = &config.TickTickSyncConfig{}
	case "todoist":
		c.Todoist = &config.TodoistSyncConfig{APIOrigin: d.Authority.APIOrigin}
	case "twenty":
		c.Twenty = &config.TwentySyncConfig{APIOrigin: d.Authority.APIOrigin, WebOrigin: d.Authority.WebOrigin}
	}
	return c
}
func trackerWorkerCode(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, tracker.ErrDenied):
		return "denied"
	case errors.Is(err, tracker.ErrInvalid):
		return "invalid"
	case errors.Is(err, db.ErrTrackerOutcomeUnknown):
		return "unknown"
	case errors.Is(err, tracker.ErrControlCapacity):
		return "capacity"
	default:
		return "unavailable"
	}
}

func (r *trackerRemoteRun) Close(ctx context.Context) error {
	if r == nil || r.runtime == nil {
		return nil
	}
	var out struct {
		Closed bool `json:"closed"`
	}
	err := r.invoke(ctx, "close", tracker.ActionRecover, struct{}{}, &out, true)
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return err
}
