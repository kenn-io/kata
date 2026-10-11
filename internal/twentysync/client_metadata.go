package twentysync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const workspaceQuery = `query KataTwentyWorkspace { currentWorkspace { id displayName } }`
const objectsQuery = `query KataTwentyObjects($after: ConnectionCursor) { objects(paging: { first: 100, after: $after }, filter: { isActive: { is: true } }) { edges { node { id nameSingular isActive } } pageInfo { hasNextPage endCursor } } }`
const fieldsQuery = `query KataTwentyFields($id: UUID!, $after: ConnectionCursor) { object(id: $id) { id nameSingular isActive fields(paging: { first: 100, after: $after }) { edges { node { id name type isActive options } } pageInfo { hasNextPage endCursor } } } }`

func (s *clientSession) Workspace(ctx context.Context, c Config) (Workspace, error) {
	if err := s.validate(c, false); err != nil {
		return Workspace{}, err
	}
	raw, err := s.graphql(ctx, workspaceQuery, nil)
	if err != nil {
		return Workspace{}, err
	}
	var envelope struct {
		Workspace *struct {
			ID          string  `json:"id"`
			DisplayName *string `json:"displayName"`
		} `json:"currentWorkspace"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Workspace == nil || envelope.Workspace.DisplayName == nil {
		return Workspace{}, fmt.Errorf("incomplete Twenty workspace identity response")
	}
	id, err := CanonicalID(envelope.Workspace.ID)
	if err != nil || (s.config.WorkspaceID != "" && id != s.config.WorkspaceID) {
		return Workspace{}, fmt.Errorf("twenty API key workspace does not match binding")
	}
	name := *envelope.Workspace.DisplayName
	if !utf8.ValidString(name) || strings.ContainsRune(name, '\x00') {
		return Workspace{}, fmt.Errorf("invalid Twenty workspace display name")
	}
	s.verified = s.config.WorkspaceID != ""
	return Workspace{ID: id, DisplayName: name}, nil
}

type pageInfo struct {
	Next   *bool   `json:"hasNextPage"`
	Cursor *string `json:"endCursor"`
}
type metadataNode struct {
	ID           string         `json:"id"`
	NameSingular string         `json:"nameSingular"`
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Active       *bool          `json:"isActive"`
	Options      jsontext.Value `json:"options"`
}
type metadataConnection struct {
	Edges *[]struct {
		Node *metadataNode `json:"node"`
	} `json:"edges"`
	Page pageInfo `json:"pageInfo"`
}

func nextCursor(info pageInfo, seen map[string]bool) (string, bool, error) {
	if info.Next == nil {
		return "", false, fmt.Errorf("incomplete Twenty pagination response")
	}
	if !*info.Next {
		return "", false, nil
	}
	if info.Cursor == nil || *info.Cursor == "" || seen[*info.Cursor] {
		return "", false, fmt.Errorf("twenty pagination cursor is missing or repeated")
	}
	seen[*info.Cursor] = true
	return *info.Cursor, true, nil
}

// ensureWorkspace checks the bound workspace once per session; a successful
// Workspace read marks the session verified.
func (s *clientSession) ensureWorkspace(ctx context.Context, c Config) error {
	if s.verified {
		return nil
	}
	_, err := s.Workspace(ctx, c)
	return err
}

// Schema validates the live task status options against the binding's mapping.
// The session loads them once; validation runs on every call because callers
// may pass a different mapping for the same source.
func (s *clientSession) Schema(ctx context.Context, c Config) (Schema, error) {
	if err := s.validate(c, true); err != nil {
		return Schema{}, err
	}
	if err := s.ensureWorkspace(ctx, c); err != nil {
		return Schema{}, err
	}
	if s.statusOptions == nil {
		options, err := s.loadStatusOptions(ctx)
		if err != nil {
			return Schema{}, err
		}
		s.statusOptions = options
	}
	schema := Schema{StatusOptions: slices.Clone(s.statusOptions)}
	if err := ValidateSchema(c, schema); err != nil {
		return Schema{}, err
	}
	return schema, nil
}

// loadStatusOptions separately traverses objects and task fields; neither
// defaults to a complete collection in Twenty. Option values are a GraphQL
// JSON scalar.
func (s *clientSession) loadStatusOptions(ctx context.Context) ([]string, error) {
	total := 0
	objectID, err := s.taskObjectID(ctx, &total)
	if err != nil {
		return nil, err
	}
	fields, err := s.taskFields(ctx, objectID, &total)
	if err != nil {
		return nil, err
	}
	for name, kind := range map[string]string{"title": "TEXT", "bodyV2": "RICH_TEXT", "status": "SELECT"} {
		if fields[name].Type != kind {
			return nil, fmt.Errorf("unsupported Twenty task schema: requires title TEXT, bodyV2 RICH_TEXT and status SELECT")
		}
	}
	var options []struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(fields["status"].Options, &options); err != nil || options == nil {
		return nil, fmt.Errorf("invalid Twenty task status options")
	}
	values := make([]string, 0, len(options))
	for _, option := range options {
		if option.Value == nil {
			return nil, fmt.Errorf("incomplete Twenty task status option")
		}
		values = append(values, *option.Value)
	}
	return values, nil
}

// taskObjectID finds the single active task object in the metadata catalog.
func (s *clientSession) taskObjectID(ctx context.Context, total *int) (string, error) {
	objectID := ""
	seenIDs := map[string]bool{}
	seenCursors := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxResponsePages {
			return "", fmt.Errorf("twenty metadata exceeds 1000 pages")
		}
		var after any
		if cursor != "" {
			after = cursor
		}
		raw, err := s.graphqlBounded(ctx, objectsQuery, map[string]any{"after": after}, total)
		if err != nil {
			return "", err
		}
		var envelope struct {
			Objects *metadataConnection `json:"objects"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Objects == nil || envelope.Objects.Edges == nil {
			return "", fmt.Errorf("incomplete Twenty object metadata")
		}
		for _, edge := range *envelope.Objects.Edges {
			if edge.Node == nil || edge.Node.Active == nil || edge.Node.NameSingular == "" {
				return "", fmt.Errorf("incomplete Twenty object metadata")
			}
			id, err := CanonicalID(edge.Node.ID)
			if err != nil || seenIDs[id] {
				return "", fmt.Errorf("invalid or duplicate Twenty object identity")
			}
			seenIDs[id] = true
			if len(seenIDs) > maxItems {
				return "", fmt.Errorf("twenty metadata exceeds 10000 objects")
			}
			if edge.Node.NameSingular == "task" && *edge.Node.Active {
				if objectID != "" {
					return "", fmt.Errorf("duplicate Twenty task object")
				}
				objectID = id
			}
		}
		next, more, err := nextCursor(envelope.Objects.Page, seenCursors)
		if err != nil {
			return "", err
		}
		if !more {
			break
		}
		cursor = next
	}
	if objectID == "" {
		return "", fmt.Errorf("twenty task object is unavailable")
	}
	return objectID, nil
}

// taskFields returns the task object's active fields by name.
func (s *clientSession) taskFields(ctx context.Context, objectID string, total *int) (map[string]metadataNode, error) {
	seenIDs := map[string]bool{}
	seenCursors := map[string]bool{}
	cursor := ""
	fields := map[string]metadataNode{}
	for page := 0; ; page++ {
		if page >= maxResponsePages {
			return nil, fmt.Errorf("twenty field metadata exceeds 1000 pages")
		}
		var after any
		if cursor != "" {
			after = cursor
		}
		raw, err := s.graphqlBounded(ctx, fieldsQuery, map[string]any{"id": objectID, "after": after}, total)
		if err != nil {
			return nil, err
		}
		var envelope struct {
			Object *struct {
				ID     string              `json:"id"`
				Name   string              `json:"nameSingular"`
				Active *bool               `json:"isActive"`
				Fields *metadataConnection `json:"fields"`
			} `json:"object"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Object == nil || envelope.Object.ID != objectID || envelope.Object.Name != "task" || envelope.Object.Active == nil || !*envelope.Object.Active || envelope.Object.Fields == nil || envelope.Object.Fields.Edges == nil {
			return nil, fmt.Errorf("incomplete or mismatched Twenty task field metadata")
		}
		for _, edge := range *envelope.Object.Fields.Edges {
			if edge.Node == nil || edge.Node.Active == nil || edge.Node.Name == "" || edge.Node.Type == "" {
				return nil, fmt.Errorf("incomplete Twenty field metadata")
			}
			id, err := CanonicalID(edge.Node.ID)
			if err != nil || seenIDs[id] {
				return nil, fmt.Errorf("invalid or duplicate Twenty field identity")
			}
			seenIDs[id] = true
			if len(seenIDs) > maxItems {
				return nil, fmt.Errorf("twenty metadata exceeds 10000 fields")
			}
			if *edge.Node.Active {
				if _, ok := fields[edge.Node.Name]; ok {
					return nil, fmt.Errorf("duplicate Twenty task field")
				}
				fields[edge.Node.Name] = *edge.Node
			}
		}
		next, more, err := nextCursor(envelope.Object.Fields.Page, seenCursors)
		if err != nil {
			return nil, err
		}
		if !more {
			return fields, nil
		}
		cursor = next
	}
}
