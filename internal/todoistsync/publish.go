package todoistsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/todoistsync/todoistapi"
)

type todoistPublishSession struct {
	session *clientSession
	config  Config
}
type todoistCreatePayload struct {
	Content     string `json:"content"`
	Description string `json:"description"`
	ProjectID   string `json:"project_id"`
}

// OpenPublish captures a provider session for the configured source.
func (a *Adapter) OpenPublish(ctx context.Context, b db.IssueSyncBinding) (issuesync.PublishSession, error) {
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	if a.fetcher == nil {
		return nil, fmt.Errorf("todoist publishing requires a fetcher")
	}
	if b.Provider != "todoist" || b.SourceKey != c.SourceKey() || b.RemoteID != c.RemoteID() {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	raw, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, err
	}
	session, ok := raw.(*clientSession)
	if !ok {
		return nil, fmt.Errorf("todoist session does not support publishing")
	}
	return &todoistPublishSession{session: session, config: c}, nil
}
func validateTodoistCreate(p todoistCreatePayload) error {
	if ValidateID(p.ProjectID) != nil {
		return issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	if strings.TrimSpace(p.Content) == "" {
		return issuesync.NewPublicationValidationError(issuesync.PublicationContentInvalid)
	}
	for _, v := range []string{p.Content, p.Description} {
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) || len(v) > 1<<20 {
			return issuesync.NewPublicationValidationError(issuesync.PublicationContentInvalid)
		}
	}
	return nil
}
func (s *todoistPublishSession) PrepareCreate(ctx context.Context, i db.Issue) (jsontext.Value, error) {
	if i.Status != "open" {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenIssueRequired)
	}
	p := todoistCreatePayload{Content: i.Title, Description: i.Body, ProjectID: s.config.ProjectID}
	if err := validateTodoistCreate(p); err != nil {
		return nil, err
	}
	if _, err := s.session.Project(ctx, s.config); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}
func (s *todoistPublishSession) Create(ctx context.Context, raw jsontext.Value, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	var p todoistCreatePayload
	if json.Unmarshal(raw, &p, json.RejectUnknownMembers(true)) != nil || admit == nil || p.ProjectID != s.config.ProjectID {
		return zero, fmt.Errorf("invalid Todoist creation request")
	}
	if err := validateTodoistCreate(p); err != nil {
		return zero, err
	}
	if _, err := s.session.Project(ctx, s.config); err != nil {
		return zero, err
	}
	if err := admit(ctx); err != nil {
		return zero, err
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	resp, err := s.session.api.CreateTaskAPIV1TasksPostWithResponse(ctx, &todoistapi.CreateTaskAPIV1TasksPostRequestOptions{Body: &todoistapi.CreateTaskAPIV1TasksPostBody{Content: p.Content, Description: &p.Description, ProjectID: &p.ProjectID}})
	// Validate scoped identity independently so an incomplete successful response
	// can still be recovered without ever repeating the create request.
	if resp == nil || resp.StatusCode != 200 {
		return zero, responseErrorOrEmpty(err)
	}
	var identity struct {
		ID        string `json:"id"`
		ProjectID string `json:"project_id"`
		Checked   *bool  `json:"checked"`
		Deleted   *bool  `json:"is_deleted"`
	}
	if json.Unmarshal(resp.Body, &identity) != nil || ValidateID(identity.ID) != nil || identity.ProjectID != s.config.ProjectID {
		return zero, fmt.Errorf("invalid or out-of-project Todoist creation response")
	}
	remote := db.IssuePublicationRemote{ExternalID: "task:" + identity.ID}
	if err != nil || resp.JSON200 == nil || identity.Checked == nil || identity.Deleted == nil {
		return remote, fmt.Errorf("todoist creation observation is incomplete")
	}
	row := taskFrom(*resp.JSON200, s.config.historyFloor())
	if row.Checked || row.Deleted {
		return remote, fmt.Errorf("todoist created task is not open")
	}
	return todoistPublicationObservation(row), nil
}
func responseErrorOrEmpty(err error) error {
	if err != nil {
		return responseError(err, "create Todoist task")
	}
	return emptyResponse("create Todoist task")
}
func todoistPublicationObservation(row Task) db.IssuePublicationRemote {
	obs := observation(row)
	r := db.IssuePublicationRemote{ExternalID: "task:" + row.ID, URL: "https://app.todoist.com/app/task/" + row.ID, Status: obs.Status, RawStatus: obs.RawStatus, ClosedReason: obs.ClosedReason, Version: obs.Version}
	if !row.UpdatedAtUnknown {
		r.SourceUpdatedAt = &obs.Version
	}
	return r
}
func (s *todoistPublishSession) ReadCreated(ctx context.Context, identity db.IssuePublicationRemote) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	id := strings.TrimPrefix(identity.ExternalID, "task:")
	if ValidateID(id) != nil || strings.ContainsAny(id, "/:?#\\") {
		return zero, fmt.Errorf("todoist adoption requires a task ID")
	}
	if _, err := s.session.Project(ctx, s.config); err != nil {
		return zero, err
	}
	row, err := s.session.currentTask(ctx, s.config, StatusTarget{ID: id})
	if err != nil {
		return zero, err
	}
	if row.ID != id || row.ProjectID != s.config.ProjectID || row.Deleted {
		return zero, fmt.Errorf("todoist task is outside the selected project")
	}
	return todoistPublicationObservation(row), nil
}

var _ issuesync.PublishAdapter = (*Adapter)(nil)
var _ issuesync.PublishSession = (*todoistPublishSession)(nil)
