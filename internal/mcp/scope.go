package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/shortid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// ScopeMode identifies how the MCP server selected its projects at startup.
type ScopeMode string

const (
	// ScopeBound fixes one project.
	ScopeBound ScopeMode = "bound"
	// ScopeAllowlist fixes a set of project UIDs.
	ScopeAllowlist ScopeMode = "allowlist"
	// ScopeAll follows the selected daemon's active project catalog.
	ScopeAll ScopeMode = "all"
)

// Scope keeps the startup authorization boundary. Fixed scopes store project
// UIDs so a later rename does not remove or broaden authority.
type Scope struct {
	mode     ScopeMode
	projects []ProjectIdentity
}

// NewBoundScope creates the compatible one-project scope.
func NewBoundScope(project ProjectIdentity) (*Scope, error) {
	if err := validateProjectIdentity(project, false); err != nil {
		return nil, err
	}
	return &Scope{mode: ScopeBound, projects: []ProjectIdentity{project}}, nil
}

// ResolveBoundScope resolves one project ID and stores its immutable UID.
func ResolveBoundScope(ctx context.Context, client *kataclient.Client, resolved ProjectIdentity) (*Scope, error) {
	if client == nil {
		return nil, errors.New("kata API client is required")
	}
	if err := validateProjectIdentity(resolved, false); err != nil {
		return nil, err
	}
	catalog, err := loadScopeProjects(ctx, client, false)
	if err != nil {
		return nil, err
	}
	for _, project := range catalog {
		if project.ID == resolved.ID {
			return NewBoundScope(project)
		}
	}
	return nil, fmt.Errorf("MCP project ID %d was not found", resolved.ID)
}

// NewAllowlistScope creates a fixed UID allowlist.
func NewAllowlistScope(projects []ProjectIdentity) (*Scope, error) {
	if len(projects) == 0 {
		return nil, errors.New("at least one MCP project is required")
	}
	copyProjects := append([]ProjectIdentity(nil), projects...)
	seenNames := make(map[string]struct{}, len(copyProjects))
	seenUIDs := make(map[string]struct{}, len(copyProjects))
	for _, project := range copyProjects {
		if err := validateProjectIdentity(project, true); err != nil {
			return nil, err
		}
		if _, exists := seenNames[project.Name]; exists {
			return nil, fmt.Errorf("duplicate MCP project %q", project.Name)
		}
		if _, exists := seenUIDs[project.UID]; exists {
			return nil, fmt.Errorf("duplicate MCP project UID %q", project.UID)
		}
		seenNames[project.Name] = struct{}{}
		seenUIDs[project.UID] = struct{}{}
	}
	return &Scope{mode: ScopeAllowlist, projects: copyProjects}, nil
}

// NewAllScope creates a daemon-wide dynamic project scope.
func NewAllScope() *Scope {
	return &Scope{mode: ScopeAll}
}

// ResolveAllowlistScope resolves names once and stores their immutable UIDs.
func ResolveAllowlistScope(ctx context.Context, client *kataclient.Client, names []string) (*Scope, error) {
	if client == nil {
		return nil, errors.New("kata API client is required")
	}
	seen := make(map[string]struct{}, len(names))
	clean := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, errors.New("MCP project name must not be empty")
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate MCP project %q", name)
		}
		seen[name] = struct{}{}
		clean = append(clean, name)
	}
	if len(clean) == 0 {
		return nil, errors.New("at least one MCP project is required")
	}
	catalog, err := loadScopeProjects(ctx, client, false)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]ProjectIdentity, len(catalog))
	for _, project := range catalog {
		byName[project.Name] = project
	}
	projects := make([]ProjectIdentity, 0, len(clean))
	for _, name := range clean {
		project, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("MCP project %q was not found", name)
		}
		projects = append(projects, project)
	}
	return NewAllowlistScope(projects)
}

// Mode returns the immutable startup mode.
func (s *Scope) Mode() ScopeMode {
	if s == nil {
		return ""
	}
	return s.mode
}

// Projects returns current project identities within the startup boundary.
func (s *Scope) Projects(ctx context.Context, client *kataclient.Client, includeArchived bool) ([]ProjectIdentity, error) {
	if s == nil {
		return nil, errors.New("MCP project scope is required")
	}
	if s.mode == ScopeBound && (client == nil || s.projects[0].UID == "") {
		return append([]ProjectIdentity(nil), s.projects...), nil
	}
	if client == nil {
		return append([]ProjectIdentity(nil), s.projects...), nil
	}
	catalog, err := loadScopeProjects(ctx, client, includeArchived)
	if err != nil {
		return nil, err
	}
	if s.mode == ScopeAll {
		return catalog, nil
	}
	allowed := make(map[string]struct{}, len(s.projects))
	for _, project := range s.projects {
		allowed[project.UID] = struct{}{}
	}
	projects := make([]ProjectIdentity, 0, len(allowed))
	for _, project := range catalog {
		if _, ok := allowed[project.UID]; ok {
			projects = append(projects, project)
		}
	}
	if len(projects) != len(allowed) {
		return nil, errors.New("one or more projects in the MCP startup scope are no longer available")
	}
	return projects, nil
}

// Project resolves a current project name within the startup boundary.
func (s *Scope) Project(ctx context.Context, client *kataclient.Client, name string, includeArchived bool) (ProjectIdentity, error) {
	name = strings.TrimSpace(name)
	projects, err := s.Projects(ctx, client, includeArchived)
	if err != nil {
		return ProjectIdentity{}, err
	}
	if name == "" && s.mode == ScopeBound {
		return projects[0], nil
	}
	for _, project := range projects {
		if project.Name == name {
			return project, nil
		}
	}
	return ProjectIdentity{}, fmt.Errorf("project %q is outside the MCP startup scope", name)
}

// IssueTarget resolves a qualified issue reference and enforces scope.
func (s *Scope) IssueTarget(ctx context.Context, client *kataclient.Client, raw string, write bool) (ProjectIdentity, string, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return ProjectIdentity{}, "", errors.New("issue reference must not be empty")
	}
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return ProjectIdentity{}, "", fmt.Errorf("issue reference %q is invalid", ref)
	}
	if s.mode != ScopeBound && parsed.Project == "" {
		if write {
			return ProjectIdentity{}, "", errors.New("multi-project writes require project#ref")
		}
		return ProjectIdentity{}, "", errors.New("multi-project issue reads require project#ref")
	}
	projectName := parsed.Project
	if s.mode == ScopeBound && projectName == "" {
		projects, projectErr := s.Projects(ctx, client, false)
		if projectErr != nil {
			return ProjectIdentity{}, "", projectErr
		}
		project := projects[0]
		return project, ref, nil
	}
	project, err := s.Project(ctx, client, projectName, false)
	if err != nil {
		return ProjectIdentity{}, "", err
	}
	return project, parsed.ShortID, nil
}

func loadScopeProjects(ctx context.Context, client *kataclient.Client, includeArchived bool) ([]ProjectIdentity, error) {
	var include *string
	if includeArchived {
		value := "archived"
		include = &value
	}
	response, err := client.ListProjects(ctx, &generated.ListProjectsRequestOptions{
		Query: &generated.ListProjectsQuery{Include: include},
	})
	if err != nil {
		return nil, err
	}
	projects := make([]ProjectIdentity, 0, len(response.Projects))
	for _, project := range response.Projects {
		projects = append(projects, ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}

func validateProjectIdentity(project ProjectIdentity, requireUID bool) error {
	if project.ID <= 0 {
		return errors.New("kata project ID is required")
	}
	if strings.TrimSpace(project.Name) == "" {
		return errors.New("kata project name is required")
	}
	if requireUID && strings.TrimSpace(project.UID) == "" {
		return errors.New("kata project UID is required")
	}
	return nil
}
