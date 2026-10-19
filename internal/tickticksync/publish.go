package tickticksync

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

type ticktickPublishSession struct{ session *clientSession }
type ticktickCreatePayload struct {
	Title     string `json:"title"`
	ProjectID string `json:"projectId"`
	Content   string `json:"content"`
}

// OpenPublish captures a provider session for the configured source.
func (a *Adapter) OpenPublish(ctx context.Context, b db.IssueSyncBinding) (issuesync.PublishSession, error) {
	c, err := bindingConfig(b)
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	if a.fetcher == nil {
		return nil, fmt.Errorf("TickTick publishing requires a fetcher")
	}
	raw, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, err
	}
	session, ok := raw.(*clientSession)
	if !ok {
		return nil, fmt.Errorf("TickTick session does not support publishing")
	}
	return &ticktickPublishSession{session: session}, nil
}
func (s *ticktickPublishSession) writableProject(ctx context.Context) error {
	var p Project
	if err := s.session.get(ctx, s.session.path(), &p); err != nil {
		return err
	}
	// Validate source availability independently of the status-sync mode so the
	// same explicit permission failure has one publication remedy in either mode.
	cfg := s.session.config
	cfg.StatusSync = "one-way"
	if err := ValidateProject(cfg, p); err != nil {
		return issuesync.NewPublicationValidationError(issuesync.PublicationDestinationUnavailable)
	}
	if p.Permission != "" && p.Permission != "write" {
		return issuesync.NewPublicationValidationError(issuesync.PublicationWritePermissionRequired)
	}
	return nil
}
func validateTickTickCreate(p ticktickCreatePayload) error {
	if ValidateID(p.ProjectID) != nil {
		return issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	if strings.TrimSpace(p.Title) == "" {
		return issuesync.NewPublicationValidationError(issuesync.PublicationContentInvalid)
	}
	for _, v := range []string{p.Title, p.Content} {
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) || len(v) > 1<<20 {
			return issuesync.NewPublicationValidationError(issuesync.PublicationContentInvalid)
		}
	}
	return nil
}
func (s *ticktickPublishSession) PrepareCreate(ctx context.Context, issue db.Issue) (jsontext.Value, error) {
	if issue.Status != "open" {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenIssueRequired)
	}
	p := ticktickCreatePayload{Title: issue.Title, ProjectID: s.session.config.ProjectID, Content: issue.Body}
	if err := validateTickTickCreate(p); err != nil {
		return nil, err
	}
	if err := s.writableProject(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}
func (s *ticktickPublishSession) Create(ctx context.Context, raw jsontext.Value, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	var p ticktickCreatePayload
	if json.Unmarshal(raw, &p, json.RejectUnknownMembers(true)) != nil || admit == nil || p.ProjectID != s.session.config.ProjectID {
		return zero, fmt.Errorf("invalid TickTick creation request")
	}
	if err := validateTickTickCreate(p); err != nil {
		return zero, err
	}
	if err := s.writableProject(ctx); err != nil {
		return zero, err
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return zero, err
	}
	if err = s.session.client.admit(ctx); err != nil {
		return zero, err
	}
	if err = admit(ctx); err != nil {
		return zero, err
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiOrigin+"/open/v1/task", bytes.NewReader(encoded))
	if err != nil {
		return zero, fmt.Errorf("invalid TickTick creation request")
	}
	req.Header.Set("Authorization", "Bearer "+s.session.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	res, err := s.session.client.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("TickTick creation response was lost")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == 429 || res.StatusCode >= 500 {
		s.session.client.deferRequests(res.Header.Get("Retry-After"))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return zero, fmt.Errorf("TickTick creation failed (HTTP %d)", res.StatusCode)
	}
	response, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil || len(response) > maxResponseBytes {
		return zero, fmt.Errorf("TickTick creation response was incomplete")
	}
	var task Task
	if json.Unmarshal(response, &task) != nil || ValidateID(task.ID) != nil || task.ProjectID != p.ProjectID {
		return zero, fmt.Errorf("invalid or out-of-project TickTick creation response")
	}
	remote := db.IssuePublicationRemote{ExternalID: "task:" + task.ID}
	observed, err := s.observation(task)
	if err != nil {
		return remote, err
	}
	if observed.Status != "open" {
		return remote, fmt.Errorf("TickTick created task is not open")
	}
	return observed, nil
}
func (s *ticktickPublishSession) observation(task Task) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	if err := validateTaskIdentity(s.session.config, task); err != nil {
		return zero, err
	}
	obs, err := statusObservation(task, s.session.client.cfg.Now())
	if err != nil {
		return zero, err
	}
	// TickTick supplies no source update clock. Publication must not seed or
	// advance the content checkpoint used by the normal importer.
	return db.IssuePublicationRemote{ExternalID: "task:" + task.ID, URL: "https://ticktick.com/webapp/#p/" + task.ProjectID + "/tasks/" + task.ID, Status: obs.Status, RawStatus: obs.RawStatus, ClosedReason: obs.ClosedReason, Version: obs.Version}, nil
}
func (s *ticktickPublishSession) ReadCreated(ctx context.Context, identity db.IssuePublicationRemote) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	id := strings.TrimPrefix(identity.ExternalID, "task:")
	if ValidateID(id) != nil {
		return zero, fmt.Errorf("TickTick adoption requires a task ID")
	}
	if _, err := s.session.Project(ctx); err != nil {
		return zero, err
	}
	task, err := s.session.readTask(ctx, id)
	if err != nil {
		return zero, err
	}
	return s.observation(task)
}

var _ issuesync.PublishAdapter = (*Adapter)(nil)
var _ issuesync.PublishSession = (*ticktickPublishSession)(nil)
