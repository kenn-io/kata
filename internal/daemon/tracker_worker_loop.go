package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
)

// RunTrackerWorkers owns provider sessions and children under the daemon
// lifetime. Poll contexts are short and never own a credential-bearing session.
// Existing enrollments/origin-pinned clients are the only network destinations.
func (s *Server) RunTrackerWorkers(ctx context.Context) {
	cfg := s.cfg
	if cfg.TrackerRuntime == nil || cfg.HostAccess != nil || cfg.FederationSigning == nil {
		return
	}
	r := cfg.TrackerRuntime
	owner := newTrackerOwnerWorker(ctx)
	var pending sync.WaitGroup
	deliveryDone := make(chan struct{})
	go func() { defer close(deliveryDone); r.runTrackerReplyDelivery(ctx, cfg, owner) }()
	defer func() { owner.closeAll(); pending.Wait(); <-deliveryDone }()
	runTrackerPollScheduler(ctx, 2*time.Second, 8, func(ctx context.Context) ([]int64, error) {
		bindings, err := cfg.DB.ListFederationBindings(ctx)
		if err != nil {
			return nil, err
		}
		ids := []int64{}
		for _, binding := range bindings {
			if binding.Role == db.FederationRoleSpoke && binding.Enabled && binding.RelayConfig != nil && !binding.RelayConfig.UpstreamRevoked {
				ids = append(ids, binding.ProjectID)
			}
		}
		owner.prune(time.Now())
		return ids, nil
	}, func(request context.Context, id int64) {
		binding, err := cfg.DB.FederationBindingByProject(request, id)
		if err != nil || binding.Role != db.FederationRoleSpoke || !binding.Enabled || binding.RelayConfig == nil || binding.RelayConfig.UpstreamRevoked {
			return
		}
		project, err := cfg.DB.ProjectByID(request, id)
		if err != nil {
			return
		}
		presence, watch := r.workerPresence(request, project, binding, owner)
		if !watch {
			return
		}
		result, err := forwardTrackerWorker(request, cfg, project, trackerWorkerBody{Presence: presence}, "poll")
		if err != nil {
			return
		}
		for _, command := range result.Commands {
			if r.registerWorkerCall(ctx, owner, command) {
				pending.Add(1)
				go func() { defer pending.Done(); r.executeWorkerCall(owner, command, cfg) }()
			}
		}
	})
}
func (r *TrackerRuntime) workerPresence(ctx context.Context, project db.Project, binding db.FederationBinding, owner *trackerOwnerWorker) (tracker.WorkerPresence, bool) {
	p := tracker.WorkerPresence{Version: 1, RootInstanceUID: binding.RelayConfig.AuthorityUID, ProjectUID: project.UID, OwnerInstanceUID: r.store.InstanceUID(), OwnerEpoch: r.epoch, ExpiresAt: time.Now().Add(110 * time.Second).Unix(), Connections: []tracker.PublicDescriptor{}}
	snapshot, err := r.current()
	if err == nil {
		p.OwnerEpoch = snapshot.incarnation
		for _, c := range snapshot.connections {
			for _, grant := range c.descriptor.Grants {
				if grant.ProjectUID == project.UID && grant.RootInstanceUID == p.RootInstanceUID {
					p.Connections = append(p.Connections, tracker.PublicDescriptor{Selection: c.descriptor.Selection, Provider: c.descriptor.Provider, Label: c.descriptor.Label, Authority: c.descriptor.Authority})
					break
				}
			}
		}
	}
	if len(p.Connections) > 0 {
		return p, true
	}
	// Retained executor receipts remain reachable after grant/config removal.
	if fences, ok := r.store.(db.TrackerFenceStore); ok {
		if _, found, err := fences.LoadTrackerExecutionFence(ctx, p.RootInstanceUID, project.UID, "executor"); err == nil && found {
			return p, true
		}
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	for _, call := range owner.calls {
		if call.command.Ticket.Ticket.ProjectUID == project.UID && !call.sent {
			return p, true
		}
	}
	return p, false
}
func (r *TrackerRuntime) registerWorkerCall(ctx context.Context, w *trackerOwnerWorker, command trackerWorkerCommand) bool {
	// Verify before reserving memory; neither arbitrary peers nor unknown pins
	// can fill the call/session queues or trigger owner credential resolution.
	validation, cancelValidation := context.WithTimeout(ctx, 10*time.Second)
	err := r.verifyWorkerTransport(validation, command.Ticket, command.Arguments)
	cancelValidation()
	if err != nil {
		return false
	}
	t := command.Ticket.Ticket
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(time.Now())
	if old, exists := w.calls[t.CallUID]; exists {
		// Re-delivery is the same admitted invocation. A different signed call
		// cannot substitute arguments beneath an existing call UID.
		if bytes.Equal(old.command.Arguments, command.Arguments) && bytes.Equal(old.command.Ticket.Signature, command.Ticket.Signature) {
			return false
		}
		return false
	}
	reserved := workerMetadataMethod(t.Method)
	count, projectCount, connectionCount := 0, 0, 0
	for _, call := range w.calls {
		old := call.command.Ticket.Ticket
		if workerMetadataMethod(old.Method) != reserved {
			continue
		}
		count++
		if old.ProjectUID == t.ProjectUID {
			projectCount++
		}
		if old.Selection.OwnerInstanceUID == t.Selection.OwnerInstanceUID && old.Selection.ConnectionUID == t.Selection.ConnectionUID {
			connectionCount++
		}
	}
	if (reserved && count >= 16) || (!reserved && (count >= 256 || projectCount >= 64 || connectionCount >= 64)) {
		return false
	}
	command.Arguments = bytes.Clone(command.Arguments)
	command.Ticket.Signature = bytes.Clone(command.Ticket.Signature)
	w.calls[t.CallUID] = &trackerOwnerCall{command: command, expires: time.Unix(t.ExpiresAt, 0).Add(2 * time.Minute)}
	return true
}
func (r *TrackerRuntime) executeWorkerCall(w *trackerOwnerWorker, command trackerWorkerCommand, cfg ServerConfig) {
	t := command.Ticket.Ticket
	callCtx, cancelCall := trackerWorkerReplyContext(w.ctx, t)
	defer cancelCall()
	ctx, c, err := r.authorizeWorkerOwner(callCtx, command.Ticket, command.Arguments)
	var result any
	if workerRecoveryMethod(t.Method) {
		err = r.verifyWorkerTransport(callCtx, command.Ticket, command.Arguments)
		var args trackerWorkerMetadataArgs
		if err == nil {
			args, err = decodeWorkerMetadata(command.Arguments)
		}
		if err == nil {
			result, err = r.ownerMetadata(callCtx, w, t, args)
		}
	} else if strings.HasSuffix(t.Method, ".close") {
		err = r.verifyWorkerTransport(callCtx, command.Ticket, command.Arguments)
		var empty struct{}
		if err == nil && json.Unmarshal(command.Arguments, &empty, json.RejectUnknownMembers(true)) != nil {
			err = tracker.ErrInvalid
		}
		if err == nil {
			err = w.closeRun(t)
		}
		result = struct {
			Closed bool `json:"closed"`
		}{true}
	} else if strings.HasSuffix(t.Method, ".discover") {
		// Discovery chooses no account and exercises no provider credential.
		// The representative selection routes to an owner; only independently
		// mapped actual subjects see their explicitly discoverable descriptors.
		err = r.verifyWorkerTransport(callCtx, command.Ticket, command.Arguments)
		var empty struct{}
		if err == nil && json.Unmarshal(command.Arguments, &empty, json.RejectUnknownMembers(true)) != nil {
			err = tracker.ErrInvalid
		}
		if err == nil {
			snapshot, snapshotErr := r.current()
			err = snapshotErr
			if err == nil && snapshot.incarnation != t.OwnerEpoch {
				err = tracker.ErrUnavailable
			}
			if err == nil {
				_, err = r.mappedContext(callCtx, snapshot, t.ProjectUID, t.Principal)
			}
			if err == nil {
				connections := []tracker.PublicDescriptor{}
				for _, connection := range snapshot.connections {
					if d, ok := connection.descriptor.Public(t.RootInstanceUID, t.ProjectUID, t.Principal); ok {
						connections = append(connections, d)
					}
				}
				result = connections
			}
		}
	} else if err == nil {
		if strings.HasSuffix(t.Method, ".authorize") {
			var empty struct{}
			if json.Unmarshal(command.Arguments, &empty, json.RejectUnknownMembers(true)) != nil {
				err = tracker.ErrInvalid
			} else {
				result = workerOwnerAuthorization(c, t)
			}
		} else {
			var args trackerWorkerArguments
			if json.Unmarshal(command.Arguments, &args, json.RejectUnknownMembers(true)) != nil {
				err = tracker.ErrInvalid
			} else {
				var session *trackerOwnerSession
				var release func()
				session, release, err = w.acquire(t, time.Now())
				if err == nil {
					// Retain the complete mapped project/actor/transaction fences.
					// Session lifetime is attached separately; poll contexts never
					// own either the method or its credential-bearing session.
					methodCtx, methodCleanup := trackerWorkerMethodContext(ctx, session, t)
					if t.Method == t.Provider+".write-status" {
						result, err = r.executeOwnerStatus(methodCtx, cfg, w, session, c, t, args)
					} else {
						result, err = session.provider.providerCall(methodCtx, c, t, args, nil)
					}
					methodCleanup()
					release()
				}
			}
		}
	}
	// Provider execution authority never arrives in an observation payload.
	if observation, ok := result.(issuesync.StatusObservation); ok {
		observation.TrackerWriteUID = ""
		result = observation
	}
	reply := trackerWorkerReply{CallUID: t.CallUID, SessionUID: t.SessionUID, Code: trackerWorkerCode(err), Failure: workerStatusFailure(err)}
	var chunks []tracker.WorkerResultChunk
	held := 0
	if err == nil {
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil || len(raw) > tracker.WorkerResultBytes {
			reply.Code = "unavailable"
		} else if len(raw) <= 48<<10 {
			reply.Data = raw
			held = len(raw)
		} else {
			manifest, parts, splitErr := tracker.SplitWorkerResult(t, raw)
			if splitErr != nil {
				reply.Code = "unavailable"
			} else {
				reply.Manifest = &manifest
				chunks = parts
				held = len(raw)
			}
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	call, found := w.calls[t.CallUID]
	if !found {
		return
	}
	projectBytes := 0
	for _, other := range w.calls {
		if other.command.Ticket.Ticket.ProjectUID == t.ProjectUID {
			projectBytes += other.held
		}
	}
	if w.retained+held > tracker.WorkerDaemonBytes || projectBytes+held > tracker.WorkerProjectBytes {
		reply = trackerWorkerReply{CallUID: t.CallUID, SessionUID: t.SessionUID, Code: "capacity"}
		chunks = nil
		held = 0
	}
	call.reply = &reply
	call.chunks = chunks
	call.held = held
	call.complete = true
	call.expires = time.Now().Add(2 * time.Minute)
	w.retained += held
	w.wakeReplies()
}
func flushTrackerWorkerReply(ctx context.Context, presence tracker.WorkerPresence, w *trackerOwnerWorker, call *trackerOwnerCall, send func(context.Context, trackerWorkerBody, string) error) {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := send(requestCtx, trackerWorkerBody{Presence: presence, Reply: call.reply}, "reply")
	if err == nil {
		for _, chunk := range call.chunks {
			if err = send(requestCtx, trackerWorkerBody{Presence: presence, Chunk: &chunk}, "chunk"); err != nil {
				break
			}
		}
	}
	cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		call.sent = true
		w.retained -= call.held
		call.held = 0
		call.chunks = nil
		call.reply = nil
		delete(w.calls, call.command.Ticket.Ticket.CallUID)
	}
	// Failure keeps the dispatcher's claim until its entire job returns. Only
	// that owner releases it; an inner release races a new dispatcher's claim.
}

func workerMetadataMethod(method string) bool {
	return workerRecoveryMethod(method) || strings.HasSuffix(method, ".authorize") || strings.HasSuffix(method, ".discover") || strings.HasSuffix(method, ".close")
}

func trackerWorkerMethodContext(ctx context.Context, session *trackerOwnerSession, t tracker.WorkerTicket) (context.Context, context.CancelFunc) {
	budget, stopBudget := trackerWorkerReplyContext(ctx, t)
	method, cancel := context.WithDeadline(budget, session.hardDeadline)
	stop := context.AfterFunc(session.ctx, cancel)
	if session.ctx.Err() != nil {
		cancel()
	}
	return method, func() { stop(); cancel(); stopBudget() }
}
