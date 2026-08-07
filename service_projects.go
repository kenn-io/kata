package kata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// ErrProjectConflict reports that a requested stable project identity or name
// is already bound to a different project.
var ErrProjectConflict = errors.New("kata: project identity conflict")

// ErrProjectNotFound reports that no host-visible project has the requested
// stable identity.
var ErrProjectNotFound = errors.New("kata: project not found")

// ProjectState describes whether a host-managed project is available for
// ordinary operations.
type ProjectState string

const (
	// ProjectActive is visible to ordinary task operations.
	ProjectActive ProjectState = "active"
	// ProjectArchived retains history but is hidden from ordinary operations.
	ProjectArchived ProjectState = "archived"
)

// Project is the stable project identity returned to an embedding host.
type Project struct {
	ID        int64
	UID       string
	Name      string
	State     ProjectState
	CreatedAt time.Time
}

// ProjectSpec identifies the exact project a host wants to exist.
type ProjectSpec struct {
	UID  string
	Name string
}

// EnsureProjectResult reports the stable project and whether this call
// created it.
type EnsureProjectResult struct {
	Project Project
	Created bool
}

// ProjectMutationResult reports the stable project and whether the call
// changed its lifecycle.
type ProjectMutationResult struct {
	Project Project
	Changed bool
}

// EnsureProject creates the exact requested project or returns the existing
// exact binding. Reusing either the UID or name for a different project fails
// with ErrProjectConflict. Archived projects are returned without reactivation.
func (s *Service) EnsureProject(ctx context.Context, spec ProjectSpec) (EnsureProjectResult, error) {
	if err := validateProjectSpec(spec); err != nil {
		return EnsureProjectResult{}, err
	}
	callCtx, done, err := s.beginHostCall(ctx)
	if err != nil {
		return EnsureProjectResult{}, err
	}
	defer done()

	if existing, found, err := s.projectByUID(callCtx, spec.UID); err != nil {
		return EnsureProjectResult{}, err
	} else if found {
		return exactProjectResult(existing, spec)
	}
	if existing, found, err := s.projectByName(callCtx, spec.Name); err != nil {
		return EnsureProjectResult{}, err
	} else if found {
		return exactProjectResult(existing, spec)
	}

	created, event, createErr := s.store.CreateProjectWithUIDAndEvent(
		callCtx, spec.Name, spec.UID, db.SystemActor,
	)
	if createErr == nil {
		s.broadcaster.Broadcast(daemon.StreamMsg{
			Kind: "event", Event: &event, ProjectID: created.ID,
		})
		s.hookSink.Enqueue(event)
		return EnsureProjectResult{Project: publicProject(created), Created: true}, nil
	}

	// Another service instance may have created the same identity after both
	// lookups. Collapse that race into the same idempotent result.
	if existing, found, lookupErr := s.projectByUID(callCtx, spec.UID); lookupErr != nil {
		return EnsureProjectResult{}, errors.Join(createErr, lookupErr)
	} else if found {
		return exactProjectResult(existing, spec)
	}
	if existing, found, lookupErr := s.projectByName(callCtx, spec.Name); lookupErr != nil {
		return EnsureProjectResult{}, errors.Join(createErr, lookupErr)
	} else if found {
		return exactProjectResult(existing, spec)
	}
	return EnsureProjectResult{}, fmt.Errorf("kata: ensure project: %w", createErr)
}

// ArchiveProject hides a project from ordinary operations while retaining its
// stable identity, task history, and events. Repeating the exact archive is an
// idempotent no-op.
func (s *Service) ArchiveProject(
	ctx context.Context,
	projectUID string,
	actor string,
) (ProjectMutationResult, error) {
	if !katauid.Valid(projectUID) || projectUID == db.SystemProjectUID {
		return ProjectMutationResult{}, ErrProjectNotFound
	}
	if strings.TrimSpace(actor) == "" {
		return ProjectMutationResult{}, errors.New("kata: archive actor is required")
	}
	callCtx, done, err := s.beginHostCall(ctx)
	if err != nil {
		return ProjectMutationResult{}, err
	}
	defer done()

	existing, found, err := s.projectByUID(callCtx, projectUID)
	if err != nil {
		return ProjectMutationResult{}, err
	}
	if !found {
		return ProjectMutationResult{}, ErrProjectNotFound
	}
	if existing.DeletedAt != nil {
		return ProjectMutationResult{Project: publicProject(existing)}, nil
	}

	archived, event, err := s.store.RemoveProject(callCtx, db.RemoveProjectParams{
		ProjectID: existing.ID, Actor: actor, Force: true,
	})
	if errors.Is(err, db.ErrProjectAlreadyArchived) {
		converged, found, lookupErr := s.projectByUID(callCtx, projectUID)
		if lookupErr != nil {
			return ProjectMutationResult{}, lookupErr
		}
		if !found {
			return ProjectMutationResult{}, ErrProjectNotFound
		}
		return ProjectMutationResult{Project: publicProject(converged)}, nil
	}
	if errors.Is(err, db.ErrNotFound) {
		return ProjectMutationResult{}, ErrProjectNotFound
	}
	if err != nil {
		return ProjectMutationResult{}, fmt.Errorf("kata: archive project: %w", err)
	}
	if event != nil {
		s.broadcaster.Broadcast(daemon.StreamMsg{
			Kind: "event", Event: event, ProjectID: archived.ID,
		})
		s.hookSink.Enqueue(*event)
	}
	return ProjectMutationResult{Project: publicProject(archived), Changed: true}, nil
}

func validateProjectSpec(spec ProjectSpec) error {
	if !katauid.Valid(spec.UID) || spec.UID == db.SystemProjectUID {
		return fmt.Errorf("kata: invalid project UID")
	}
	if spec.Name == db.SystemProjectName || strings.Contains(spec.Name, "#") {
		return fmt.Errorf("kata: invalid project name")
	}
	if err := config.ValidateProjectName(spec.Name); err != nil {
		return fmt.Errorf("kata: invalid project name: %w", err)
	}
	return nil
}

func (s *Service) projectByUID(ctx context.Context, uid string) (db.Project, bool, error) {
	project, err := s.store.ProjectByUID(ctx, uid)
	if errors.Is(err, db.ErrNotFound) {
		return db.Project{}, false, nil
	}
	if err != nil {
		return db.Project{}, false, fmt.Errorf("kata: find project by UID: %w", err)
	}
	return project, true, nil
}

func (s *Service) projectByName(ctx context.Context, name string) (db.Project, bool, error) {
	project, err := s.store.ProjectByNameIncludingArchived(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return db.Project{}, false, nil
	}
	if err != nil {
		return db.Project{}, false, fmt.Errorf("kata: find project by name: %w", err)
	}
	return project, true, nil
}

func exactProjectResult(existing db.Project, spec ProjectSpec) (EnsureProjectResult, error) {
	if existing.UID != spec.UID || existing.Name != spec.Name {
		return EnsureProjectResult{}, projectConflict(spec)
	}
	return EnsureProjectResult{Project: publicProject(existing)}, nil
}

func projectConflict(spec ProjectSpec) error {
	return fmt.Errorf("%w: requested UID %q and name %q do not identify the same project",
		ErrProjectConflict, spec.UID, spec.Name)
}

func publicProject(project db.Project) Project {
	state := ProjectActive
	if project.DeletedAt != nil {
		state = ProjectArchived
	}
	return Project{
		ID: project.ID, UID: project.UID, Name: project.Name,
		State: state, CreatedAt: project.CreatedAt,
	}
}

func (s *Service) beginHostCall(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("kata: context is required")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, errors.New("kata: service is closed")
	}
	s.handlerWG.Add(1)
	lifetimeCtx := s.lifetimeCtx
	s.mu.Unlock()

	callCtx, cancel := context.WithCancel(ctx)
	stopLifetimeCancel := context.AfterFunc(lifetimeCtx, cancel)
	done := func() {
		stopLifetimeCancel()
		cancel()
		s.handlerWG.Done()
	}
	return callCtx, done, nil
}
