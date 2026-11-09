package daemon

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/twentysync"
)

// Only current provider interface values are transported. In particular no
// owner config/client, arbitrary URL/header, Notion Page parent claim or error
// chain can be encoded as arguments.
type trackerWorkerArguments struct {
	GitHub        *githubsync.Config        `json:"github,omitempty"`
	Notion        *notionsync.Config        `json:"notion,omitempty"`
	Linear        *linearsync.Config        `json:"linear,omitempty"`
	Plane         *planesync.Config         `json:"plane,omitempty"`
	TickTick      *tickticksync.Config      `json:"ticktick,omitempty"`
	Todoist       *todoistsync.Config       `json:"todoist,omitempty"`
	Twenty        *twentysync.Config        `json:"twenty,omitempty"`
	ID            string                    `json:"id,omitempty"`
	Number        int                       `json:"number,omitempty"`
	Page          int                       `json:"page,omitempty"`
	Since         *time.Time                `json:"since,omitempty"`
	Until         *time.Time                `json:"until,omitempty"`
	ParentRequest *githubsync.ParentRequest `json:"parent_request,omitempty"`
	TodoistTarget *todoistsync.StatusTarget `json:"todoist_target,omitempty"`
	Target        string                    `json:"target,omitempty"`
}
type trackerWorkerRepositoryComments struct {
	Comments map[int][]githubsync.Comment `json:"comments"`
	Complete bool                         `json:"complete"`
}
type trackerWorkerStatusLocators struct {
	Locators []githubsync.StatusLocator `json:"locators"`
	Next     int                        `json:"next"`
}

// trackerOwnerRun is serialized by its owning worker session lease. A source
// change replaces the underlying read session; one root run reuses the current
// session across status/content instead of consuming two connection slots.
type trackerOwnerRun struct {
	session          any
	sessionKey       string
	notionSource     notionsync.DataSource
	notionPages      map[string]notionsync.Page
	githubRepository githubsync.Repository
}

func (a trackerWorkerArguments) countConfigs() int {
	count := 0
	for _, present := range []bool{a.GitHub != nil, a.Notion != nil, a.Linear != nil, a.Plane != nil, a.TickTick != nil, a.Todoist != nil, a.Twenty != nil} {
		if present {
			count++
		}
	}
	return count
}
func (s *trackerOwnerRun) providerCall(ctx context.Context, c trackerLocalConnection, t tracker.WorkerTicket, a trackerWorkerArguments, admit func() error) (any, error) {
	method := strings.TrimPrefix(t.Method, t.Provider+".")
	if method == "write-status" && (admit == nil || t.Action != tracker.ActionStatus) {
		return nil, tracker.ErrDenied
	}
	if a.countConfigs() > 1 {
		return nil, tracker.ErrInvalid
	}
	switch t.Provider {
	case "github":
		return s.githubCall(ctx, c, t, a, method, admit)
	case "notion":
		if a.Notion == nil && a.countConfigs() != 0 {
			return nil, tracker.ErrInvalid
		}
		return s.notionCall(ctx, c, t, a, method, admit)
	case "linear":
		if a.Linear == nil || (t.SourceKey != "" && t.SourceKey != a.Linear.SourceKey()) || a.Linear.SourceKey() != t.Resource {
			return nil, tracker.ErrDenied
		}
		cfg := *a.Linear
		if s.session == nil || s.sessionKey != cfg.SourceKey() {
			opened, err := c.clients.linear.ForRun(ctx, cfg)
			if err != nil {
				return nil, err
			}
			s.session, s.sessionKey = opened, cfg.SourceKey()
		}
		session, ok := s.session.(linearsync.Session)
		if !ok {
			return nil, tracker.ErrUnavailable
		}
		switch method {
		case "scope":
			return session.Scope(ctx, cfg)
		case "states":
			return session.States(ctx, cfg)
		case "issues":
			return session.Issues(ctx, cfg)
		case "read-status", "write-status":
			status, ok := session.(linearsync.StatusSession)
			if !ok {
				return nil, tracker.ErrUnavailable
			}
			if method == "read-status" {
				return status.ReadStatus(ctx, cfg, a.ID)
			}
			return status.WriteStatus(ctx, cfg, a.ID, a.Target, admit)
		}
	case "plane":
		if a.Plane == nil || (t.SourceKey != "" && t.SourceKey != a.Plane.SourceKey()) || a.Plane.APIOrigin != c.descriptor.Authority.APIOrigin || a.Plane.SourceKey() != t.Resource {
			return nil, tracker.ErrDenied
		}
		cfg := *a.Plane
		if s.session == nil || s.sessionKey != cfg.SourceKey() {
			opened, err := c.clients.plane.ForRun(ctx, cfg)
			if err != nil {
				return nil, err
			}
			s.session, s.sessionKey = opened, cfg.SourceKey()
		}
		session, ok := s.session.(planesync.Session)
		if !ok {
			return nil, tracker.ErrUnavailable
		}
		switch method {
		case "project":
			return session.Project(ctx, cfg)
		case "states":
			return session.States(ctx, cfg)
		case "work-items":
			return session.WorkItems(ctx, cfg)
		case "read-status", "write-status":
			status, ok := session.(planesync.StatusSession)
			if !ok {
				return nil, tracker.ErrUnavailable
			}
			if method == "read-status" {
				return status.ReadStatus(ctx, cfg, a.ID)
			}
			return status.WriteStatus(ctx, cfg, a.ID, a.Target, admit)
		}
	case "ticktick":
		if a.TickTick == nil || (t.SourceKey != "" && t.SourceKey != a.TickTick.SourceKey()) || a.TickTick.SourceKey() != t.Resource {
			return nil, tracker.ErrDenied
		}
		cfg := *a.TickTick
		if s.session == nil || s.sessionKey != cfg.SourceKey() {
			opened, err := c.clients.ticktick.ForRun(ctx, cfg)
			if err != nil {
				return nil, err
			}
			s.session, s.sessionKey = opened, cfg.SourceKey()
		}
		session, ok := s.session.(tickticksync.Session)
		if !ok {
			return nil, tracker.ErrUnavailable
		}
		switch method {
		case "project":
			return session.Project(ctx)
		case "data":
			return session.Data(ctx)
		case "task":
			return session.Task(ctx, a.ID)
		case "read-status", "write-status":
			status, ok := session.(tickticksync.StatusSession)
			if !ok {
				return nil, tracker.ErrUnavailable
			}
			if method == "read-status" {
				return status.ReadStatus(ctx, a.ID)
			}
			return status.WriteStatus(ctx, a.ID, a.Target, admit)
		}
	case "todoist":
		if method == "account" {
			if a.countConfigs() != 0 || !strings.HasPrefix(t.Resource, "todoist:"+c.descriptor.Authority.APIOrigin+"/") {
				return nil, tracker.ErrInvalid
			}
			return c.clients.todoist.Account(ctx)
		}
		if a.Todoist == nil || (t.SourceKey != "" && t.SourceKey != a.Todoist.SourceKey()) || a.Todoist.APIOrigin != c.descriptor.Authority.APIOrigin {
			return nil, tracker.ErrDenied
		}
		cfg := *a.Todoist
		if cfg.SourceKey() != t.Resource && "todoist:"+cfg.APIOrigin+"/project:"+cfg.ProjectID != t.Resource {
			return nil, tracker.ErrDenied
		}
		if s.session == nil || s.sessionKey != cfg.SourceKey() {
			opened, err := c.clients.todoist.ForRun(ctx, cfg)
			if err != nil {
				return nil, err
			}
			s.session, s.sessionKey = opened, cfg.SourceKey()
		}
		session, ok := s.session.(todoistsync.Session)
		if !ok {
			return nil, tracker.ErrUnavailable
		}
		switch method {
		case "project":
			return session.Project(ctx, cfg)
		case "tasks":
			if a.Since == nil || a.Until == nil {
				return nil, tracker.ErrInvalid
			}
			return session.Tasks(ctx, cfg, *a.Since, *a.Until)
		case "read-status", "write-status":
			status, ok := session.(todoistsync.StatusSession)
			if !ok || a.TodoistTarget == nil {
				return nil, tracker.ErrUnavailable
			}
			if method == "read-status" {
				return status.ReadStatus(ctx, cfg, *a.TodoistTarget)
			}
			return status.WriteStatus(ctx, cfg, *a.TodoistTarget, a.Target, admit)
		}
	case "twenty":
		if a.Twenty == nil || (t.SourceKey != "" && t.SourceKey != a.Twenty.SourceKey()) || a.Twenty.APIOrigin != c.descriptor.Authority.APIOrigin || a.Twenty.WebOrigin != c.descriptor.Authority.WebOrigin {
			return nil, tracker.ErrDenied
		}
		cfg := *a.Twenty
		if cfg.SourceKey() != t.Resource && !(method == "workspace" && cfg.WorkspaceID == "" && strings.HasPrefix(t.Resource, "twenty:"+cfg.APIOrigin+"/")) {
			return nil, tracker.ErrDenied
		}
		if s.session == nil || s.sessionKey != cfg.SourceKey() {
			opened, err := c.clients.twenty.ForRun(ctx, cfg)
			if err != nil {
				return nil, err
			}
			s.session, s.sessionKey = opened, cfg.SourceKey()
		}
		session, ok := s.session.(twentysync.Session)
		if !ok {
			return nil, tracker.ErrUnavailable
		}
		switch method {
		case "workspace":
			workspace, err := session.Workspace(ctx, cfg)
			if err != nil {
				return nil, err
			}
			if "twenty:"+cfg.APIOrigin+"/"+workspace.ID != t.Resource {
				return nil, tracker.ErrDenied
			}
			return workspace, nil
		case "schema":
			return session.Schema(ctx, cfg)
		case "tasks":
			return session.Tasks(ctx, cfg)
		case "read-status", "write-status":
			status, ok := session.(twentysync.StatusSession)
			if !ok {
				return nil, tracker.ErrUnavailable
			}
			if method == "read-status" {
				return status.ReadStatus(ctx, cfg, a.ID)
			}
			return status.WriteStatus(ctx, cfg, a.ID, a.Target, admit)
		}
	}
	return nil, tracker.ErrInvalid
}
func (s *trackerOwnerRun) notionCall(ctx context.Context, c trackerLocalConnection, t tracker.WorkerTicket, a trackerWorkerArguments, method string, admit func() error) (any, error) {
	source, ok := strings.CutPrefix(t.Resource, "notion:")
	if !ok || source == "" || (t.SourceKey != "" && t.SourceKey != t.Resource) {
		return nil, tracker.ErrDenied
	}
	if a.Notion != nil && a.Notion.DataSourceID != source {
		return nil, tracker.ErrDenied
	}
	if s.session == nil {
		opened, err := c.clients.notion.ForRun(ctx)
		if err != nil {
			return nil, err
		}
		s.session = opened
		s.sessionKey = t.Resource
	}
	session, ok := s.session.(notionsync.Session)
	if !ok || s.sessionKey != t.Resource {
		return nil, tracker.ErrDenied
	}
	if s.notionSource.ID == "" {
		schema, err := session.DataSource(ctx, source)
		if err != nil {
			return nil, err
		}
		if schema.ID != source || schema.DatabaseID == "" {
			return nil, tracker.ErrDenied
		}
		s.notionSource = schema
	}
	if a.Notion != nil && a.Notion.DatabaseID != s.notionSource.DatabaseID {
		return nil, tracker.ErrDenied
	}
	switch method {
	case "data-source":
		if a.ID != source {
			return nil, tracker.ErrDenied
		}
		return s.notionSource, nil
	case "database":
		if a.ID != s.notionSource.DatabaseID {
			return nil, tracker.ErrDenied
		}
		parent, err := session.Database(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if parent.ID != a.ID {
			return nil, tracker.ErrDenied
		}
		return parent, nil
	case "pages":
		if a.Notion == nil {
			return nil, tracker.ErrInvalid
		}
		pages, err := session.Pages(ctx, *a.Notion, a.Since)
		if err != nil {
			return nil, err
		}
		verified := make(map[string]notionsync.Page, len(pages))
		for _, page := range pages {
			if page.DataSourceID != source || page.DatabaseID != s.notionSource.DatabaseID {
				return nil, tracker.ErrDenied
			}
			verified[page.ID] = page
		}
		s.notionPages = verified
		return pages, nil
	case "content":
		if a.Notion == nil {
			return nil, tracker.ErrInvalid
		}
		page, known := s.notionPages[a.ID]
		if !known {
			return nil, tracker.ErrDenied
		}
		return session.Content(ctx, *a.Notion, page)
	case "read-status", "write-status":
		if a.Notion == nil {
			return nil, tracker.ErrInvalid
		}
		status, ok := session.(notionsync.StatusSession)
		if !ok {
			return nil, tracker.ErrUnavailable
		}
		if method == "read-status" {
			return status.ReadStatus(ctx, *a.Notion, a.ID)
		}
		return status.WriteStatus(ctx, *a.Notion, a.ID, a.Target, admit)
	}
	return nil, tracker.ErrInvalid
}
func (s *trackerOwnerRun) githubCall(ctx context.Context, c trackerLocalConnection, t tracker.WorkerTicket, a trackerWorkerArguments, method string, admit func() error) (any, error) {
	if a.GitHub == nil {
		return nil, tracker.ErrInvalid
	}
	cfg := *a.GitHub
	binding := cfg.Binding()
	key := strings.ToLower(binding.Host + "/" + binding.Owner + "/" + binding.Repo)
	if strings.HasPrefix(t.Resource, "github:repository:") && t.Resource != "github:repository:"+key {
		return nil, tracker.ErrDenied
	}
	if s.session == nil || s.sessionKey != key {
		opened := c.clients.github
		if scoped, ok := opened.(githubsync.BindingSessionFetcher); ok {
			var err error
			opened, err = scoped.ForBinding(ctx, binding)
			if err != nil {
				return nil, err
			}
		}
		repository, err := opened.Repository(ctx, binding.Host, binding.Owner, binding.Repo)
		if err != nil {
			return nil, err
		}
		if (t.SourceKey != "" && t.SourceKey != "github:"+repository.NodeID) || (t.Resource != "github:repository:"+key && t.Resource != "github:"+repository.NodeID) {
			return nil, tracker.ErrDenied
		}
		s.session = opened
		s.sessionKey = key
		s.githubRepository = repository
	}
	if (t.SourceKey != "" && t.SourceKey != "github:"+s.githubRepository.NodeID) || (t.Resource != "github:repository:"+key && t.Resource != "github:"+s.githubRepository.NodeID) {
		return nil, tracker.ErrDenied
	}
	session, ok := s.session.(githubsync.Fetcher)
	if !ok {
		return nil, tracker.ErrUnavailable
	}
	switch method {
	case "repository":
		return s.githubRepository, nil
	case "issues":
		return session.Issues(ctx, binding, a.Since)
	case "comments":
		return session.Comments(ctx, binding, a.Number)
	case "repository-comments":
		comments, complete, err := session.RepositoryComments(ctx, binding, a.Since)
		return trackerWorkerRepositoryComments{Comments: comments, Complete: complete}, err
	case "parents":
		if a.ParentRequest == nil {
			return nil, tracker.ErrInvalid
		}
		return session.ParentData(ctx, binding, *a.ParentRequest)
	case "status-locators":
		status, ok := session.(githubsync.StatusLocatorSession)
		if !ok {
			return nil, tracker.ErrUnavailable
		}
		locators, next, err := status.StatusLocators(ctx, cfg, a.Page)
		return trackerWorkerStatusLocators{Locators: locators, Next: next}, err
	case "read-status", "write-status":
		status, ok := session.(githubsync.StatusSession)
		if !ok {
			return nil, tracker.ErrUnavailable
		}
		if method == "read-status" {
			return status.ReadStatus(ctx, cfg, a.ID, a.Number)
		}
		return status.WriteStatus(ctx, cfg, a.ID, a.Number, a.Target, admit)
	}
	return nil, tracker.ErrInvalid
}
