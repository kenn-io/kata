package linearsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

const publishedFields = `id url updatedAt archivedAt trashed completedAt canceledAt team { id } project { id } state { id }`
const createQuery = `mutation KataLinearCreate($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { ` + publishedFields + ` } } }`
const publishedQuery = `query KataLinearPublished($id: String!) { issue(id: $id) { ` + publishedFields + ` } }`

type linearCreatePayload struct {
	TeamID      string `json:"teamId"`
	ProjectID   string `json:"projectId,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description"`
	StateID     string `json:"stateId"`
}
type linearPublishSession struct {
	session *clientSession
	config  Config
}

// OpenPublish captures a provider session for the configured source.
func (a *Adapter) OpenPublish(ctx context.Context, b db.IssueSyncBinding) (issuesync.PublishSession, error) {
	if a.fetcher == nil {
		return nil, fmt.Errorf("linear publishing requires a fetcher")
	}
	cfg, err := bindingConfig(b)
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	raw, err := a.fetcher.ForRun(ctx, cfg)
	if err != nil {
		return nil, err
	}
	session, ok := raw.(*clientSession)
	if !ok {
		return nil, fmt.Errorf("linear session does not support publishing")
	}
	return &linearPublishSession{session: session, config: cfg}, nil
}
func (s *linearPublishSession) PrepareCreate(ctx context.Context, issue db.Issue) (jsontext.Value, error) {
	if issue.Status != "open" {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenIssueRequired)
	}
	payload := linearCreatePayload{TeamID: s.config.TeamID, ProjectID: s.config.ProjectID, Title: issue.Title, Description: issue.Body}
	if err := validateLinearCreate(payload); err != nil {
		return nil, err
	}
	if _, err := s.session.Scope(ctx, s.config); err != nil {
		return nil, err
	}
	states, err := s.session.States(ctx, s.config)
	if err != nil {
		return nil, err
	}
	schema, err := resolveStatusSchema(states)
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationSchemaInvalid)
	}
	payload.StateID, err = schema.target(s.config, "open")
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenTargetRequired)
	}
	return json.Marshal(payload)
}
func validateLinearCreate(payload linearCreatePayload) error {
	if strings.TrimSpace(payload.Title) == "" || !utf8.ValidString(payload.Title) || !utf8.ValidString(payload.Description) || strings.ContainsAny(payload.Title+payload.Description, "\x00") || len(payload.Description) > maxDescriptionBytes {
		return issuesync.NewPublicationValidationError(issuesync.PublicationLinearContentInvalid)
	}
	return nil
}
func (s *linearPublishSession) Create(ctx context.Context, raw jsontext.Value, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	var input linearCreatePayload
	if json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)) != nil || admit == nil {
		return zero, fmt.Errorf("invalid Linear creation request")
	}
	if input.TeamID != s.config.TeamID || input.ProjectID != s.config.ProjectID {
		return zero, fmt.Errorf("linear creation scope changed")
	}
	if err := validateLinearCreate(input); err != nil {
		return zero, err
	}
	schema, err := s.session.statusSchema(ctx, s.config)
	if err != nil {
		return zero, err
	}
	if schema.types[input.StateID] != "unstarted" {
		return zero, fmt.Errorf("linear creation state is no longer open")
	}
	payload, err := json.Marshal(map[string]any{"query": createQuery, "variables": map[string]any{"input": input}})
	if err != nil {
		return zero, err
	}
	data, mutationErr := s.session.mutate(ctx, payload, func() error { return admit(ctx) })
	var result struct {
		Create *struct {
			Success *bool          `json:"success"`
			Issue   jsontext.Value `json:"issue"`
		} `json:"issueCreate"`
	}
	if json.Unmarshal(data, &result) != nil || result.Create == nil {
		return zero, fmt.Errorf("linear creation outcome is unproven")
	}
	remote, observationErr := linearPublicationObservation(s.config, schema, result.Create.Issue, "")
	if mutationErr != nil {
		return remote, mutationErr
	}
	if result.Create.Success == nil || !*result.Create.Success {
		return remote, fmt.Errorf("linear creation was not confirmed")
	}
	return remote, observationErr
}
func (s *linearPublishSession) ReadCreated(ctx context.Context, identity db.IssuePublicationRemote) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	id, err := CanonicalID(strings.TrimPrefix(identity.ExternalID, "issue:"))
	if err != nil {
		return zero, fmt.Errorf("linear adoption requires an issue UUID")
	}
	schema, err := s.session.statusSchema(ctx, s.config)
	if err != nil {
		return zero, err
	}
	raw, err := s.session.query(ctx, publishedQuery, map[string]any{"id": id})
	if err != nil {
		return zero, err
	}
	var data struct {
		Issue jsontext.Value `json:"issue"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return zero, fmt.Errorf("invalid Linear publication read")
	}
	return linearPublicationObservation(s.config, schema, data.Issue, id)
}
func linearPublicationObservation(cfg Config, schema statusSchema, raw jsontext.Value, expected string) (db.IssuePublicationRemote, error) {
	var remote db.IssuePublicationRemote
	var row issueWire
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &row) != nil || json.Unmarshal(raw, &fields) != nil || row.Team == nil {
		return remote, fmt.Errorf("invalid Linear publication identity")
	}
	id, err := CanonicalID(row.ID)
	if err != nil || (expected != "" && id != expected) || row.Team.ID != cfg.TeamID {
		return remote, fmt.Errorf("linear publication identity or team changed")
	}
	if _, ok := fields["project"]; !ok || cfg.ProjectID != "" && (row.Project == nil || row.Project.ID != cfg.ProjectID) {
		return remote, fmt.Errorf("linear publication project changed")
	}
	remote.ExternalID = "issue:" + id
	issue, err := decodeIssue(raw, cfg, false)
	if err != nil {
		return remote, err
	}
	if issue.Trashed || issue.ArchivedAt != nil {
		return remote, fmt.Errorf("linear publication is archived or trashed")
	}
	typ, ok := schema.types[issue.StateID]
	if !ok {
		return remote, fmt.Errorf("linear publication state is absent from the workflow")
	}
	obs, err := statusObservation(issue, typ)
	if err != nil {
		return remote, err
	}
	remote.Status = obs.Status
	remote.RawStatus = obs.RawStatus
	remote.ClosedReason = obs.ClosedReason
	remote.Version = obs.Version
	remote.SourceUpdatedAt = &issue.UpdatedAt
	if issue.URL != "" {
		if !issueURL(issue.URL) {
			return remote, fmt.Errorf("invalid Linear publication URL")
		}
		remote.URL = issue.URL
	}
	return remote, nil
}

var _ issuesync.PublishAdapter = (*Adapter)(nil)
var _ issuesync.PublishSession = (*linearPublishSession)(nil)
