package daemon

import (
	"context"
	"encoding/json/jsontext"
	"reflect"
	"slices"
	"sync"
	"time"

	"go.kenn.io/kata/internal/tracker"
)

// Provider arguments/results are strictly typed by the selected method at the
// owner. Raw JSON here is bounded transport, not a general HTTP/config proxy.
type trackerWorkerCommand struct {
	Ticket    tracker.SignedWorkerTicket `json:"ticket"`
	Arguments jsontext.Value             `json:"arguments"`
}
type trackerWorkerReply struct {
	Failure    *trackerWorkerFailure         `json:"failure,omitempty"`
	CallUID    string                        `json:"call_uid"`
	SessionUID string                        `json:"session_uid"`
	Code       string                        `json:"code"`
	Data       jsontext.Value                `json:"data,omitempty"`
	Manifest   *tracker.WorkerResultManifest `json:"manifest,omitempty"`
}
type trackerWorkerPending struct {
	write     *trackerRootWorkerWrite
	ctx       context.Context // one root invocation; released on return/cancellation
	command   trackerWorkerCommand
	result    chan trackerWorkerReply
	reserved  bool
	completed bool
	assembler *tracker.WorkerResultAssembler
	manifest  *tracker.WorkerResultManifest
	held      int
}
type trackerWorkerBroker struct {
	mu       sync.Mutex
	calls    map[string]*trackerWorkerPending
	presence map[string]tracker.WorkerPresence
	retained int
}

func newTrackerWorkerBroker() *trackerWorkerBroker {
	return &trackerWorkerBroker{calls: map[string]*trackerWorkerPending{}, presence: map[string]tracker.WorkerPresence{}}
}
func (b *trackerWorkerBroker) pruneLocked() {
	for id, c := range b.calls {
		if c.ctx.Err() != nil {
			b.releaseLocked(id)
		}
	}
}
func (b *trackerWorkerBroker) enqueue(ctx context.Context, c trackerWorkerCommand, reserved bool, writes ...*trackerRootWorkerWrite) (*trackerWorkerPending, error) {
	if len(writes) > 1 {
		return nil, tracker.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(c.Arguments) > 48<<10 || !c.Arguments.IsValid() {
		return nil, tracker.ErrInvalid
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	t := c.Ticket.Ticket
	if _, exists := b.calls[t.CallUID]; exists {
		return nil, tracker.ErrControlConflict
	}
	total, project, connection := 0, 0, 0
	for _, pending := range b.calls {
		if pending.reserved != reserved {
			continue
		}
		total++
		pt := pending.command.Ticket.Ticket
		if pt.ProjectUID == t.ProjectUID {
			project++
		}
		if pt.Selection.OwnerInstanceUID == t.Selection.OwnerInstanceUID && pt.Selection.ConnectionUID == t.Selection.ConnectionUID {
			connection++
		}
	}
	if (reserved && total >= 16) || (!reserved && (total >= 256 || project >= 64 || connection >= 64)) {
		return nil, tracker.ErrControlCapacity
	}
	c.Arguments = slices.Clone(c.Arguments)
	c.Ticket.Signature = slices.Clone(c.Ticket.Signature)
	pending := &trackerWorkerPending{ctx: ctx, command: c, result: make(chan trackerWorkerReply, 1), reserved: reserved}
	if len(writes) == 1 {
		pending.write = writes[0]
	}
	b.calls[t.CallUID] = pending
	return pending, nil
}
func (b *trackerWorkerBroker) release(call string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.releaseLocked(call)
}
func (b *trackerWorkerBroker) poll(project, owner string, now time.Time) []trackerWorkerCommand {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	out := []trackerWorkerCommand{}
	for _, p := range b.calls {
		t := p.command.Ticket.Ticket
		if !p.completed && t.ProjectUID == project && t.Selection.OwnerInstanceUID == owner && now.Unix() < t.ExpiresAt {
			c := p.command
			c.Arguments = slices.Clone(c.Arguments)
			c.Ticket.Signature = slices.Clone(c.Ticket.Signature)
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b trackerWorkerCommand) int {
		if a.Ticket.Ticket.CallUID < b.Ticket.Ticket.CallUID {
			return -1
		}
		if a.Ticket.Ticket.CallUID > b.Ticket.Ticket.CallUID {
			return 1
		}
		return 0
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}
func (b *trackerWorkerBroker) reply(project, owner string, reply trackerWorkerReply) error {
	if (reply.Failure != nil && (!reply.Failure.valid() || reply.Code == "ok")) || len(reply.Data) > 48<<10 || (len(reply.Data) > 0 && !reply.Data.IsValid()) || (reply.Manifest != nil && len(reply.Data) > 0) {
		return tracker.ErrInvalid
	}
	switch reply.Code {
	case "ok", "denied", "unavailable", "invalid", "unknown", "capacity":
	default:
		return tracker.ErrInvalid
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	p, ok := b.calls[reply.CallUID]
	if !ok {
		return tracker.ErrUnavailable
	}
	t := p.command.Ticket.Ticket
	if t.ProjectUID != project || t.Selection.OwnerInstanceUID != owner || t.SessionUID != reply.SessionUID {
		return tracker.ErrDenied
	}
	if p.completed {
		return nil
	}
	if reply.Manifest != nil {
		if reply.Code != "ok" {
			return tracker.ErrInvalid
		}
		if p.assembler != nil {
			if reflect.DeepEqual(p.manifest, reply.Manifest) {
				return nil
			}
			return tracker.ErrControlConflict
		}
		assembler, err := tracker.NewWorkerResultAssembler(*reply.Manifest, t)
		if err != nil {
			return err
		}
		held := 2 * reply.Manifest.Size // assembly plus final copied buffer; encoded chunks have their own ingress bound
		projectBytes := 0
		for _, call := range b.calls {
			if call.command.Ticket.Ticket.ProjectUID == project {
				projectBytes += call.held
			}
		}
		if b.retained+held > tracker.WorkerDaemonBytes || projectBytes+held > tracker.WorkerProjectBytes {
			return tracker.ErrControlCapacity
		}
		copied := *reply.Manifest
		copied.Chunks = slices.Clone(copied.Chunks)
		p.manifest = &copied
		p.assembler = assembler
		p.held = held
		b.retained += held
		return nil
	}
	if p.assembler != nil {
		return tracker.ErrControlConflict
	}
	// Inline data is retained in the result channel until caller release too.
	// Charge before cloning so an existing full assembly cannot be exceeded.
	held := len(reply.Data)
	projectBytes := 0
	for _, call := range b.calls {
		if call.command.Ticket.Ticket.ProjectUID == project {
			projectBytes += call.held
		}
	}
	if b.retained+held > tracker.WorkerDaemonBytes || projectBytes+held > tracker.WorkerProjectBytes {
		return tracker.ErrControlCapacity
	}
	p.held = held
	b.retained += held
	if reply.Failure != nil {
		copyFailure := *reply.Failure
		reply.Failure = &copyFailure
	}
	reply.Data = slices.Clone(reply.Data)
	p.completed = true
	p.result <- reply
	return nil
}

func (b *trackerWorkerBroker) remember(p tracker.WorkerPresence, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, presence := range b.presence {
		if presence.ExpiresAt <= now.Unix() {
			delete(b.presence, key)
		}
	}
	key := p.ProjectUID + "." + p.OwnerInstanceUID
	projectOwners, projectConnections := 0, 0
	for otherKey, other := range b.presence {
		if otherKey != key && other.ProjectUID == p.ProjectUID {
			projectOwners++
			projectConnections += len(other.Connections)
		}
	}
	if projectOwners >= 64 || projectConnections+len(p.Connections) > 128 {
		return tracker.ErrControlCapacity
	}
	if _, exists := b.presence[key]; !exists && len(b.presence) >= 1024 {
		return tracker.ErrControlCapacity
	}
	p.Path = slices.Clone(p.Path)
	p.Connections = slices.Clone(p.Connections)
	b.presence[key] = p
	return nil
}
func (b *trackerWorkerBroker) owners(project string, now time.Time) []tracker.WorkerPresence {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []tracker.WorkerPresence{}
	for _, p := range b.presence {
		if p.ProjectUID == project && p.ExpiresAt > now.Unix() {
			p.Path = slices.Clone(p.Path)
			p.Connections = slices.Clone(p.Connections)
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b tracker.WorkerPresence) int {
		if a.OwnerInstanceUID < b.OwnerInstanceUID {
			return -1
		}
		if a.OwnerInstanceUID > b.OwnerInstanceUID {
			return 1
		}
		return 0
	})
	return out
}

func (b *trackerWorkerBroker) releaseLocked(call string) {
	if p, ok := b.calls[call]; ok {
		b.retained -= p.held
		if p.assembler != nil {
			p.assembler.Close()
		}
		delete(b.calls, call)
	}
}
func (b *trackerWorkerBroker) chunk(project, owner string, chunk tracker.WorkerResultChunk) error {
	if len(chunk.Data) > tracker.WorkerChunkBytes {
		return tracker.ErrInvalid
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	p, ok := b.calls[chunk.CallUID]
	if !ok {
		return tracker.ErrUnavailable
	}
	t := p.command.Ticket.Ticket
	if t.ProjectUID != project || chunk.RootInstanceUID != t.RootInstanceUID || t.Selection.OwnerInstanceUID != owner || chunk.ProjectUID != project || chunk.OwnerInstanceUID != owner || chunk.SessionUID != t.SessionUID {
		return tracker.ErrDenied
	}
	if p.completed {
		return nil
	}
	if p.assembler == nil {
		return tracker.ErrInvalid
	}
	if err := p.assembler.Add(chunk); err != nil {
		return err
	}
	if chunk.Ordinal == len(p.manifest.Chunks)-1 {
		raw, err := p.assembler.Complete()
		if err != nil {
			return err
		}
		p.assembler.Close()
		p.assembler = nil
		p.completed = true
		p.result <- trackerWorkerReply{CallUID: t.CallUID, SessionUID: t.SessionUID, Code: "ok", Data: raw}
	}
	return nil
}
