package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
)

func trackerRemoteServiceCandidates(snapshot *trackerRuntimeSnapshot, root, project string) ([]tracker.Principal, error) {
	out := []tracker.Principal{}
	for _, m := range snapshot.mappings {
		if m.ProjectUID == project && m.Principal.Kind == tracker.SubjectService && m.Principal.AuthorityInstanceUID == root {
			out = append(out, m.Principal)
		}
	}
	if len(out) > 16 {
		return nil, tracker.ErrAmbiguous
	}
	slices.SortFunc(out, func(a, b tracker.Principal) int { return strings.Compare(a.Subject, b.Subject) })
	return out, nil
}
func (r *TrackerRuntime) remoteMemberConnection(ctx context.Context, project string, p tracker.Principal, selection tracker.Selection, action tracker.Action, resource string) (context.Context, trackerLocalConnection, error) {
	snapshot, err := r.current()
	if err != nil {
		return ctx, trackerLocalConnection{}, err
	}
	ctx, err = r.mappedContext(ctx, snapshot, project, p)
	if err != nil {
		return ctx, trackerLocalConnection{}, err
	}
	run, err := r.remoteRun(snapshot, project, p, selection, resource)
	if err != nil {
		return ctx, trackerLocalConnection{}, err
	}
	run.action = action
	descriptor, err := run.authorize(ctx, action)
	if err != nil {
		return ctx, trackerLocalConnection{}, err
	}
	authority := trackerRunAuthority{root: r.store.InstanceUID(), project: project, principal: p, source: resource, action: action, incarnation: snapshot.incarnation, remote: run}
	ctx = context.WithValue(ctx, trackerRunAuthorityKey{}, authority)
	return tracker.WithRunCloser(tracker.WithRunSelection(ctx, selection), run.Close), remoteTrackerConnection(run, descriptor), nil
}
func remoteTrackerConnection(run *trackerRemoteRun, d tracker.PublicDescriptor) trackerLocalConnection {
	return trackerLocalConnection{config: trackerRemoteConfig(d), descriptor: tracker.Descriptor{Selection: d.Selection, Provider: d.Provider, Label: d.Label, Authority: d.Authority}, clients: newTrackerRemoteClients(run)}
}
func (r *TrackerRuntime) routeRemoteBinding(ctx context.Context, snapshot *trackerRuntimeSnapshot, binding db.IssueSyncBinding, selection tracker.Selection) (context.Context, error) {
	project, err := r.store.ProjectByID(ctx, binding.ProjectID)
	if err != nil {
		return ctx, err
	}
	candidates, err := trackerRemoteServiceCandidates(snapshot, r.store.InstanceUID(), project.UID)
	if err != nil {
		return ctx, err
	}
	type choice struct {
		run *trackerRemoteRun
		ctx context.Context
		err error
	}
	results := make(chan choice, len(candidates))
	slots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, p := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				results <- choice{err: ctx.Err()}
				return
			}
			scoped, err := r.mappedContext(ctx, snapshot, project.UID, p)
			if err != nil {
				results <- choice{err: err}
				return
			}
			resource := trackerBindingResourceAlias(binding)
			if resource == "" {
				resource = binding.SourceKey
			}
			run, err := r.remoteRun(snapshot, project.UID, p, selection, resource)
			if err == nil {
				run.source = binding.SourceKey
				run.action = tracker.ActionRead
				_, err = run.authorize(scoped, tracker.ActionRead)
			}
			results <- choice{run: run, ctx: scoped, err: err}
		}()
	}
	wg.Wait()
	close(results)
	var selected *trackerRemoteRun
	var selectedCtx context.Context
	for result := range results {
		if result.err != nil {
			if errors.Is(result.err, tracker.ErrDenied) {
				continue
			}
			return ctx, result.err
		}
		if selected != nil {
			return ctx, tracker.ErrAmbiguous
		}
		selected = result.run
		selectedCtx = result.ctx
	}
	if selected == nil {
		return ctx, tracker.ErrDenied
	}
	authority := trackerRunAuthority{root: r.store.InstanceUID(), project: project.UID, principal: selected.principal, source: binding.SourceKey, resource: trackerBindingResourceAlias(binding), action: tracker.ActionRead, incarnation: snapshot.incarnation, remote: selected}
	selectedCtx = context.WithValue(selectedCtx, trackerRunAuthorityKey{}, authority)
	selectedCtx = issuesync.WithNamedStatusCoordinator(selectedCtx, r)
	return tracker.WithRunCloser(tracker.WithRunSelection(selectedCtx, selection), selected.Close), nil
}
func (r *TrackerRuntime) selectedRemoteConnection(ctx context.Context, authority trackerRunAuthority, selection tracker.Selection, provider string, action tracker.Action) (trackerLocalConnection, error) {
	run := authority.remote
	if run == nil || run.snapshot == nil || run.runtime != r || run.project != authority.project || run.principal != authority.principal || run.snapshot.incarnation != authority.incarnation || run.descriptor.Selection != selection || run.descriptor.Provider != provider {
		return trackerLocalConnection{}, tracker.ErrUnavailable
	}
	if action == tracker.ActionRead && authority.action == tracker.ActionBind {
		action = tracker.ActionBind
	}
	descriptor, err := run.authorize(ctx, action)
	if err != nil {
		return trackerLocalConnection{}, err
	}
	return remoteTrackerConnection(run, descriptor), nil
}

// selectedMethodConnection is used only by typed fetchers and their method
// guards. Each ensuing owner method independently checks live project/grants;
// a second authorization RPC would spend the control deadline without fencing
// that later method. Authority-only bind/service/status checks keep using
// selectedConnection and therefore still require explicit owner authorization.
func (r *TrackerRuntime) selectedMethodConnection(ctx context.Context, provider string, action tracker.Action) (trackerLocalConnection, error) {
	if r == nil || r.store == nil {
		return trackerLocalConnection{}, tracker.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return trackerLocalConnection{}, err
	}
	selection, ok := tracker.RunSelection(ctx)
	if !ok || selection.Validate() != nil {
		return trackerLocalConnection{}, tracker.ErrUnavailable
	}
	if selection.OwnerInstanceUID == r.store.InstanceUID() {
		return r.selectedConnection(ctx, provider, action)
	}
	if action != tracker.ActionRead && action != tracker.ActionStatus {
		return trackerLocalConnection{}, tracker.ErrInvalid
	}
	snapshot, err := r.current()
	if err != nil {
		return trackerLocalConnection{}, err
	}
	authority, ok := ctx.Value(trackerRunAuthorityKey{}).(trackerRunAuthority)
	run := authority.remote
	if !ok || authority.incarnation != snapshot.incarnation || run == nil || run.snapshot == nil || run.runtime != r || run.project != authority.project || run.principal != authority.principal || run.snapshot.incarnation != authority.incarnation || run.descriptor.Selection != selection || run.descriptor.Provider != provider {
		return trackerLocalConnection{}, tracker.ErrUnavailable
	}
	if _, err := r.mappedContext(ctx, snapshot, authority.project, authority.principal); err != nil {
		return trackerLocalConnection{}, err
	}
	return remoteTrackerConnection(run, run.descriptor), nil
}

func (r *TrackerRuntime) remoteConnections(ctx context.Context, snapshot *trackerRuntimeSnapshot, project string, p tracker.Principal) []tracker.PublicDescriptor {
	owners := r.workerBroker.owners(project, time.Now())
	out := []tracker.PublicDescriptor{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, owner := range owners {
		if len(owner.Connections) == 0 {
			continue
		}
		selection := owner.Connections[0].Selection
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			run, err := r.remoteRun(snapshot, project, p, selection, "project:"+project)
			if err != nil {
				return
			}
			queryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			var connections []tracker.PublicDescriptor
			if run.invoke(queryCtx, "discover", tracker.ActionDiscover, struct{}{}, &connections, true) != nil || len(connections) > 128 {
				return
			}
			for _, d := range connections {
				if d.Selection.Validate() != nil || d.Selection.OwnerInstanceUID != selection.OwnerInstanceUID || !tracker.SupportedProvider(d.Provider) || d.Authority.Validate() != nil || !tracker.ValidIdentityText(d.Label, 128) {
					return
				}
			}
			mu.Lock()
			out = append(out, connections...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
