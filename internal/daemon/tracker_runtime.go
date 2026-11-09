package daemon

import (
	"context"
	"slices"
	"sync"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

type trackerLocalConnection struct {
	config     config.TrackerConnectionConfig
	descriptor tracker.Descriptor
	policy     tracker.RegistryPolicy
	clients    trackerClients
}
type trackerRuntimeSnapshot struct {
	connections map[string]trackerLocalConnection
	mappings    []config.TrackerAccessMapping
	incarnation string
}

// TrackerRuntime owns immutable account policy snapshots. It never serializes
// owner config. Reconfiguration suspends old sessions before validating/replacing
// authority; an error keeps named use unavailable while legacy sync is separate.
type TrackerRuntime struct {
	workerBroker *trackerWorkerBroker
	controlsMu   sync.Mutex
	controls     map[string]struct{}
	store        db.Storage
	signing      *RootSigning
	epoch        string
	mu           sync.RWMutex
	reload       sync.Mutex
	snapshot     *trackerRuntimeSnapshot
}

func NewTrackerRuntime(ctx context.Context, store db.Storage, connections []config.TrackerConnectionConfig, mappings []config.TrackerAccessMapping, signing ...*RootSigning) (*TrackerRuntime, error) {
	if len(signing) > 1 {
		return nil, tracker.ErrInvalid
	}
	epoch, err := uid.New()
	if err != nil {
		return nil, err
	}
	r := &TrackerRuntime{store: store, epoch: epoch, workerBroker: newTrackerWorkerBroker()}
	if len(signing) == 1 {
		r.signing = signing[0]
	}
	if err := r.Reconfigure(ctx, connections, mappings); err != nil {
		// Preserve a suspended runtime so daemon reads and a corrected owner
		// reload remain available after restore or generation-history refusal.
		return r, err
	}
	return r, nil
}
func (r *TrackerRuntime) Suspend() { r.mu.Lock(); r.snapshot = nil; r.mu.Unlock() }
func (r *TrackerRuntime) Reconfigure(ctx context.Context, input []config.TrackerConnectionConfig, mappings []config.TrackerAccessMapping) error {
	r.reload.Lock()
	defer r.reload.Unlock()
	previous, _ := r.current()
	r.Suspend()
	if r.store == nil {
		return tracker.ErrUnavailable
	}
	normalized, err := config.NormalizeTrackerConnections(input)
	if err != nil {
		return err
	}
	if err := config.ValidateTrackerAccess(mappings); err != nil {
		return err
	}
	registry, ok := r.store.(db.TrackerRegistryStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	policies := make([]tracker.RegistryPolicy, 0, len(normalized))
	connections := map[string]trackerLocalConnection{}
	for _, c := range normalized {
		policy, err := c.RegistryPolicy(r.store.InstanceUID())
		if err != nil {
			return err
		}
		policy, err = trackerPolicyWithMappings(policy, c, mappings)
		if err != nil {
			return err
		}
		policies = append(policies, policy)
		clients := newTrackerClients(c)
		if previous != nil {
			if old, ok := previous.connections[c.UID]; ok && old.policy == policy {
				clients = old.clients
			}
		}
		connections[c.UID] = trackerLocalConnection{config: c, descriptor: c.Descriptor(r.store.InstanceUID()), policy: policy, clients: clients}
	}
	// Authority is published only after its whole persisted registry commits.
	if _, err := registry.ReconcileTrackerRegistry(ctx, r.epoch, policies); err != nil {
		return err
	}
	incarnation, err := uid.New()
	if err != nil {
		return err
	}
	next := &trackerRuntimeSnapshot{connections: connections, mappings: slices.Clone(mappings), incarnation: incarnation}
	r.mu.Lock()
	r.snapshot = next
	r.mu.Unlock()
	return nil
}
func (r *TrackerRuntime) current() (*trackerRuntimeSnapshot, error) {
	if r == nil {
		return nil, tracker.ErrUnavailable
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		return nil, tracker.ErrUnavailable
	}
	return r.snapshot, nil
}

// mappedContext intersects explicit root mapping with live project/team policy.
// Host services require a verified tracker adapter; standalone runtime does not
// invent one. Callers reject unsupported host modes before invoking this seam.
func (r *TrackerRuntime) mappedContext(ctx context.Context, snapshot *trackerRuntimeSnapshot, projectUID string, p tracker.Principal) (context.Context, error) {
	if allowed, restricted := db.AuthorizedProjects(ctx); restricted && !slices.Contains(allowed, projectUID) {
		return ctx, tracker.ErrDenied
	}
	actor := ""
	for _, m := range snapshot.mappings {
		if m.ProjectUID == projectUID && m.Principal == p {
			if actor != "" {
				return ctx, tracker.ErrAmbiguous
			}
			actor = m.Actor
		}
	}
	if actor == "" {
		return ctx, tracker.ErrDenied
	}
	accessible, err := r.store.AccessibleProjectUIDs(ctx, actor)
	if err != nil {
		return ctx, err
	}
	if !slices.Contains(accessible, projectUID) {
		return ctx, tracker.ErrDenied
	}
	ctx = db.WithAuthorizedProjects(ctx, []string{projectUID})
	ctx = db.WithAdditionalTransactionFence(ctx, r.store.ProjectAccessTransactionFence(actor, []string{projectUID}))
	signer, _, attributed := db.RootAttributionFromContext(ctx)
	if r.signing != nil {
		signer, attributed = r.signing.Signer(), true
	}
	if attributed {
		if signer.AuthorityUID != r.store.InstanceUID() {
			return ctx, tracker.ErrUnavailable
		}
		ctx = db.WithRootAttribution(ctx, signer, actor)
	}
	return ctx, nil
}
func (r *TrackerRuntime) Connections(ctx context.Context, projectUID string, p tracker.Principal) ([]tracker.PublicDescriptor, error) {
	snapshot, err := r.current()
	if err != nil {
		return nil, err
	}
	if _, err := r.mappedContext(ctx, snapshot, projectUID, p); err != nil {
		return nil, err
	}
	out := r.remoteConnections(ctx, snapshot, projectUID, p)
	for _, c := range snapshot.connections {
		if public, ok := c.descriptor.Public(r.store.InstanceUID(), projectUID, p); ok {
			out = append(out, public)
		}
	}
	slices.SortFunc(out, func(a, b tracker.PublicDescriptor) int {
		if a.Selection.OwnerInstanceUID < b.Selection.OwnerInstanceUID {
			return -1
		}
		if a.Selection.OwnerInstanceUID > b.Selection.OwnerInstanceUID {
			return 1
		}
		if a.Selection.ConnectionUID < b.Selection.ConnectionUID {
			return -1
		}
		if a.Selection.ConnectionUID > b.Selection.ConnectionUID {
			return 1
		}
		return 0
	})
	return out, nil
}

// RouteBinding is the shared runner seam for scheduled and manual named runs.
// It requires independent root-owned service mapping and owner grant. The
// creating member token is never retained as background execution authority.
func (r *TrackerRuntime) RouteBinding(ctx context.Context, binding db.IssueSyncBinding) (context.Context, error) {
	if err := db.ValidateIssueSyncConnectionMarker(binding.ConnectionUID, binding.Config); err != nil {
		return ctx, err
	}
	selection, err := db.IssueSyncConnectionFromConfig(binding.Config)
	if err != nil || selection == nil {
		return ctx, tracker.ErrUnavailable
	}
	snapshot, err := r.current()
	if err != nil {
		return ctx, err
	}
	if selection.OwnerInstanceUID != r.store.InstanceUID() {
		return r.routeRemoteBinding(ctx, snapshot, binding, *selection)
	}
	connection, ok := snapshot.connections[selection.ConnectionUID]
	if !ok || connection.descriptor.Selection != *selection || connection.descriptor.Provider != binding.Provider {
		return ctx, tracker.ErrUnavailable
	}
	project, err := r.store.ProjectByID(ctx, binding.ProjectID)
	if err != nil {
		return ctx, err
	}
	// The root's own access map decides the accountable service actor. A
	// remote descriptor cannot assign it or substitute a same-name member.
	var service *tracker.Principal
	for _, m := range snapshot.mappings {
		p := m.Principal
		if m.ProjectUID != project.UID || p.Kind != tracker.SubjectService || p.AuthorityInstanceUID != r.store.InstanceUID() || !trackerBindingGrant(connection.descriptor, r.store.InstanceUID(), project.UID, p, tracker.ActionRead, binding) {
			continue
		}
		if service != nil {
			return ctx, tracker.ErrAmbiguous
		}
		candidate := p
		service = &candidate
	}
	if service == nil {
		return ctx, tracker.ErrDenied
	}
	ctx, err = r.mappedContext(ctx, snapshot, project.UID, *service)
	if err != nil {
		return ctx, err
	}
	authority := trackerRunAuthority{root: r.store.InstanceUID(), project: project.UID, principal: *service, source: binding.SourceKey, resource: trackerBindingResourceAlias(binding), action: tracker.ActionRead, incarnation: snapshot.incarnation}
	ctx = context.WithValue(ctx, trackerRunAuthorityKey{}, authority)
	ctx = issuesync.WithNamedStatusCoordinator(ctx, r)
	return tracker.WithRunSelection(ctx, *selection), nil
}

type trackerRunAuthorityKey struct{}
type trackerRunAuthority struct {
	remote                                       *trackerRemoteRun
	root, project, source, resource, incarnation string
	principal                                    tracker.Principal
	action                                       tracker.Action
}

// selectedConnection runs before every selected client/session operation. A
// retained session cannot bypass reload, generation, grant or project revocation.
func (r *TrackerRuntime) selectedConnection(ctx context.Context, provider string, action tracker.Action) (trackerLocalConnection, error) {
	selection, ok := tracker.RunSelection(ctx)
	if !ok || selection.Validate() != nil {
		return trackerLocalConnection{}, tracker.ErrUnavailable
	}
	snapshot, err := r.current()
	if err != nil {
		return trackerLocalConnection{}, err
	}
	authority, ok := ctx.Value(trackerRunAuthorityKey{}).(trackerRunAuthority)
	if !ok || authority.incarnation != snapshot.incarnation {
		return trackerLocalConnection{}, tracker.ErrUnavailable
	}
	if selection.OwnerInstanceUID != r.store.InstanceUID() {
		return r.selectedRemoteConnection(ctx, authority, selection, provider, action)
	}
	c, ok := snapshot.connections[selection.ConnectionUID]
	if !ok || c.descriptor.Selection != selection || c.descriptor.Provider != provider {
		return trackerLocalConnection{}, tracker.ErrUnavailable
	}
	if action == tracker.ActionRead && authority.action == tracker.ActionBind {
		action = tracker.ActionBind
	}
	if !c.descriptor.Allows(authority.root, authority.project, authority.principal, action, authority.source) && (authority.resource == "" || !c.descriptor.Allows(authority.root, authority.project, authority.principal, action, authority.resource)) {
		return trackerLocalConnection{}, tracker.ErrDenied
	}
	if _, err := r.mappedContext(ctx, snapshot, authority.project, authority.principal); err != nil {
		return trackerLocalConnection{}, err
	}
	return c, nil
}

func trackerBindingRouter(cfg ServerConfig) func(context.Context, db.IssueSyncBinding) (context.Context, error) {
	if cfg.TrackerRuntime == nil || cfg.HostAccess != nil {
		return nil
	}
	return cfg.TrackerRuntime.RouteBinding
}

// withSnapshotAdmission serializes the last native admission with reload's
// suspension/publication. No provider request runs under this lock. A reload
// after this boundary revokes new admissions; already admitted work may finish.
// Do not call current/selectedConnection from admit (RWMutex is not recursive).
func (r *TrackerRuntime) withSnapshotAdmission(ctx context.Context, admit func() error) error {
	if r == nil || admit == nil {
		return tracker.ErrUnavailable
	}
	authority, ok := ctx.Value(trackerRunAuthorityKey{}).(trackerRunAuthority)
	if !ok {
		return tracker.ErrUnavailable
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil || r.snapshot.incarnation != authority.incarnation {
		return tracker.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return admit()
}
