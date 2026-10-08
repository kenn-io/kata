package linearsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"time"
)

const scopeQuery = `query KataLinearScope($team: String!) { viewer { organization { id } } team(id: $team) { id name archivedAt organization { id } } }`
const projectQuery = `query KataLinearProject($project: String!, $team: ID!) { project(id: $project) { id name archivedAt trashed teams(first: 100, filter: { id: { eq: $team } }) { nodes { id } pageInfo { hasNextPage endCursor } } } }`
const statesQuery = `query KataLinearStates($after: String, $filter: WorkflowStateFilter!) { workflowStates(first: 100, after: $after, filter: $filter) { nodes { id type position archivedAt team { id } } pageInfo { hasNextPage endCursor } } }`
const issueFields = `id identifier title description url priority createdAt updatedAt archivedAt trashed completedAt canceledAt team { id } project { id } state { id } creator { id } assignee { id }`
const issuesQuery = `query KataLinearIssues($after: String, $filter: IssueFilter!) { issues(first: 100, after: $after, filter: $filter) { nodes { ` + issueFields + ` } pageInfo { hasNextPage endCursor } } }`

type identity struct {
	ID string `json:"id"`
}

func (s *clientSession) Scope(ctx context.Context, c Config) (Scope, error) {
	if err := s.validate(c); err != nil {
		return Scope{}, err
	}
	raw, err := s.query(ctx, scopeQuery, map[string]any{"team": c.TeamID})
	if err != nil {
		return Scope{}, err
	}
	var data struct {
		Viewer *struct {
			Organization identity `json:"organization"`
		} `json:"viewer"`
		Team *struct {
			ID           string     `json:"id"`
			Name         string     `json:"name"`
			ArchivedAt   *time.Time `json:"archivedAt"`
			Organization identity   `json:"organization"`
		} `json:"team"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Viewer == nil || data.Team == nil || data.Viewer.Organization.ID != c.WorkspaceID || data.Team.ID != c.TeamID || data.Team.Organization.ID != c.WorkspaceID || data.Team.ArchivedAt != nil {
		return Scope{}, fmt.Errorf("linear workspace or team is unavailable or does not match")
	}
	scope := Scope{WorkspaceID: c.WorkspaceID, TeamID: c.TeamID, ProjectID: c.ProjectID, Name: data.Team.Name}
	if c.ProjectID != "" {
		raw, err = s.query(ctx, projectQuery, map[string]any{"project": c.ProjectID, "team": c.TeamID})
		if err != nil {
			return Scope{}, err
		}
		var data struct {
			Project *struct {
				ID         string     `json:"id"`
				Name       string     `json:"name"`
				ArchivedAt *time.Time `json:"archivedAt"`
				Trashed    *bool      `json:"trashed"`
				Teams      struct {
					Nodes []identity `json:"nodes"`
				} `json:"teams"`
			} `json:"project"`
		}
		if json.Unmarshal(raw, &data) != nil || data.Project == nil || data.Project.ID != c.ProjectID || data.Project.ArchivedAt != nil || data.Project.Trashed != nil && *data.Project.Trashed {
			return Scope{}, fmt.Errorf("linear project is unavailable or does not match")
		}
		found := false
		for _, team := range data.Project.Teams.Nodes {
			if team.ID == c.TeamID {
				found = true
			}
		}
		if !found {
			return Scope{}, fmt.Errorf("linear project does not belong to selected team")
		}
		scope.Name = data.Project.Name
	}
	return scope, nil
}
func readCollection(ctx context.Context, s *clientSession, query, key string, filter map[string]any) ([]jsontext.Value, error) {
	var rows []jsontext.Value
	cursor := ""
	seen := map[string]bool{}
	size := 0
	for range maxPages {
		var after any
		if cursor != "" {
			after = cursor
		}
		raw, err := s.query(ctx, query, map[string]any{"after": after, "filter": filter})
		if err != nil {
			return nil, err
		}
		size += len(raw)
		if size > 128<<20 {
			return nil, fmt.Errorf("linear collection exceeds 128 MiB")
		}
		var data map[string]jsontext.Value
		if json.Unmarshal(raw, &data) != nil {
			return nil, fmt.Errorf("invalid Linear collection")
		}
		var conn struct {
			Nodes    *[]jsontext.Value `json:"nodes"`
			PageInfo *struct {
				HasNextPage *bool   `json:"hasNextPage"`
				EndCursor   *string `json:"endCursor"`
			} `json:"pageInfo"`
		}
		if json.Unmarshal(data[key], &conn) != nil || conn.Nodes == nil || conn.PageInfo == nil || conn.PageInfo.HasNextPage == nil {
			return nil, fmt.Errorf("incomplete Linear pagination")
		}
		if len(rows)+len(*conn.Nodes) > maxItems {
			return nil, fmt.Errorf("linear collection exceeds 10000 records")
		}
		rows = append(rows, (*conn.Nodes)...)
		if !*conn.PageInfo.HasNextPage {
			return rows, nil
		}
		if conn.PageInfo.EndCursor == nil || *conn.PageInfo.EndCursor == "" || seen[*conn.PageInfo.EndCursor] {
			return nil, fmt.Errorf("missing or repeated Linear pagination cursor")
		}
		cursor = *conn.PageInfo.EndCursor
		seen[cursor] = true
	}
	return nil, fmt.Errorf("linear pagination exceeds 1000 pages")
}
func teamFilter(c Config) map[string]any {
	return map[string]any{"team": map[string]any{"id": map[string]any{"eq": c.TeamID}}}
}
func (s *clientSession) States(ctx context.Context, c Config) ([]State, error) {
	if err := s.validate(c); err != nil {
		return nil, err
	}
	rows, err := readCollection(ctx, s, statesQuery, "workflowStates", teamFilter(c))
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(rows))
	for _, raw := range rows {
		var row struct {
			ID         string     `json:"id"`
			Type       string     `json:"type"`
			Position   *float64   `json:"position"`
			ArchivedAt *time.Time `json:"archivedAt"`
			Team       identity   `json:"team"`
		}
		if json.Unmarshal(raw, &row) != nil || row.Position == nil || row.Team.ID != c.TeamID {
			return nil, fmt.Errorf("invalid Linear workflow response")
		}
		id, err := CanonicalID(row.ID)
		if err != nil {
			return nil, err
		}
		if row.ArchivedAt != nil {
			continue
		}
		states = append(states, State{ID: id, Type: row.Type, Position: *row.Position})
	}
	if _, err := stateTypes(states); err != nil {
		return nil, err
	}
	return states, nil
}

type issueWire struct {
	ID          string     `json:"id"`
	Identifier  string     `json:"identifier"`
	URL         string     `json:"url"`
	Title       *string    `json:"title"`
	Description *string    `json:"description"`
	Priority    *float64   `json:"priority"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
	Trashed     *bool      `json:"trashed"`
	CompletedAt *time.Time `json:"completedAt"`
	CanceledAt  *time.Time `json:"canceledAt"`
	Team        *identity  `json:"team"`
	State       *identity  `json:"state"`
	Project     *identity  `json:"project"`
	Creator     *identity  `json:"creator"`
	Assignee    *identity  `json:"assignee"`
}

func decodeIssue(raw jsontext.Value, c Config, content bool) (Issue, error) {
	var fields map[string]jsontext.Value
	var row issueWire
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &row) != nil || row.Team == nil || row.State == nil {
		return Issue{}, fmt.Errorf("invalid Linear issue response")
	}
	keys := []string{"id", "team", "project", "state", "updatedAt", "archivedAt", "trashed", "completedAt", "canceledAt"}
	if content {
		keys = append(keys, "identifier", "url", "title", "description", "priority", "createdAt", "creator", "assignee")
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return Issue{}, fmt.Errorf("incomplete Linear issue response")
		}
	}
	id, err := CanonicalID(row.ID)
	if err != nil {
		return Issue{}, err
	}
	state, err := CanonicalID(row.State.ID)
	if err != nil {
		return Issue{}, err
	}
	i := Issue{ID: id, StateID: state, TeamID: row.Team.ID, Identifier: row.Identifier, URL: row.URL, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ArchivedAt: row.ArchivedAt, CompletedAt: row.CompletedAt, CanceledAt: row.CanceledAt}
	i.Trashed = row.Trashed != nil && *row.Trashed
	for _, pair := range []struct {
		in  *identity
		out *string
	}{{row.Project, &i.ProjectID}, {row.Creator, &i.CreatorID}, {row.Assignee, &i.AssigneeID}} {
		if pair.in != nil {
			*pair.out, err = CanonicalID(pair.in.ID)
			if err != nil {
				return Issue{}, err
			}
		}
	}
	if !issueScope(c, i) || !validTime(i.UpdatedAt) {
		return Issue{}, fmt.Errorf("linear issue is outside selected scope or has invalid version")
	}
	if content {
		if row.Title == nil || row.Priority == nil || math.Trunc(*row.Priority) != *row.Priority || *row.Priority < 0 || *row.Priority > 4 {
			return Issue{}, fmt.Errorf("incomplete Linear issue content")
		}
		i.Title = *row.Title
		i.Priority = int(*row.Priority)
		if row.Description != nil {
			i.Description = *row.Description
		}
	}
	return i, nil
}
func (s *clientSession) Issues(ctx context.Context, c Config) ([]Issue, error) {
	if err := s.validate(c); err != nil {
		return nil, err
	}
	filter := teamFilter(c)
	if c.ProjectID != "" {
		filter["project"] = map[string]any{"id": map[string]any{"eq": c.ProjectID}}
	}
	if c.Since != "" {
		filter["updatedAt"] = map[string]any{"gt": c.Since}
	}
	rows, err := readCollection(ctx, s, issuesQuery, "issues", filter)
	if err != nil {
		return nil, err
	}
	items := make([]Issue, 0, len(rows))
	seen := map[string]bool{}
	for _, raw := range rows {
		i, err := decodeIssue(raw, c, true)
		if err != nil {
			return nil, err
		}
		if seen[i.ID] {
			return nil, fmt.Errorf("duplicate Linear issue identity")
		}
		seen[i.ID] = true
		if i.ArchivedAt == nil && !i.Trashed {
			items = append(items, i)
		}
	}
	return items, nil
}
