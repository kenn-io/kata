package twentysync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

type twentyPublishSession struct {
	session *clientSession
	config  Config
}
type twentyCreatePayload struct {
	Title string `json:"title"`
	Body  struct {
		Markdown string `json:"markdown"`
	} `json:"bodyV2"`
	Status string `json:"status"`
}

// OpenPublish captures a provider session for the configured source.
func (a *Adapter) OpenPublish(ctx context.Context, b db.IssueSyncBinding) (issuesync.PublishSession, error) {
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	if a.fetcher == nil {
		return nil, fmt.Errorf("twenty publishing requires a fetcher")
	}
	if b.Provider != "twenty" || b.SourceKey != c.SourceKey() || b.RemoteID != c.RemoteID() {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	raw, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, err
	}
	session, ok := raw.(*clientSession)
	if !ok {
		return nil, fmt.Errorf("twenty session does not support publishing")
	}
	return &twentyPublishSession{session: session, config: c}, nil
}
func (s *twentyPublishSession) validateSchema(ctx context.Context) error {
	if err := s.session.ensureWorkspace(ctx, s.config); err != nil {
		return err
	}
	total := 0
	object, err := s.session.taskObjectID(ctx, &total)
	if err != nil {
		return err
	}
	query := strings.Replace(fieldsQuery, "isActive options", "isActive options isSystem isNullable defaultValue", 1)
	fields, err := s.session.taskFieldsWithQuery(ctx, object, &total, query)
	if err != nil {
		return err
	}
	for name, kind := range map[string]string{"title": "TEXT", "bodyV2": "RICH_TEXT", "status": "SELECT"} {
		if fields[name].Type != kind {
			return issuesync.NewPublicationValidationError(issuesync.PublicationSchemaInvalid)
		}
	}
	for name, f := range fields {
		if name == "title" || name == "bodyV2" || name == "status" {
			continue
		}
		if f.IsSystem == nil || f.IsNullable == nil {
			return issuesync.NewPublicationValidationError(issuesync.PublicationSchemaInvalid)
		}
		if !*f.IsSystem && !*f.IsNullable && (len(f.DefaultValue) == 0 || string(f.DefaultValue) == "null") {
			return issuesync.NewPublicationValidationError(issuesync.PublicationTwentyRequiredField)
		}
	}
	var options []struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(fields["status"].Options, &options) != nil {
		return issuesync.NewPublicationValidationError(issuesync.PublicationSchemaInvalid)
	}
	values := make([]string, 0, len(options))
	for _, o := range options {
		values = append(values, o.Value)
	}
	if err := ValidateSchema(s.config, Schema{StatusOptions: values}); err != nil {
		return issuesync.NewPublicationValidationError(issuesync.PublicationSchemaInvalid)
	}
	if !slices.Contains(values, s.config.OpenStatus) {
		return issuesync.NewPublicationValidationError(issuesync.PublicationOpenTargetRequired)
	}
	s.session.statusOptions = values
	return nil
}
func validateTwentyCreate(p twentyCreatePayload) error {
	if strings.TrimSpace(p.Title) == "" || !utf8.ValidString(p.Title) || strings.ContainsRune(p.Title, 0) || len(p.Title) > maxMarkdownBytes || !utf8.ValidString(p.Body.Markdown) || strings.ContainsRune(p.Body.Markdown, 0) || len(p.Body.Markdown) > maxMarkdownBytes {
		return issuesync.NewPublicationValidationError(issuesync.PublicationContentInvalid)
	}
	return nil
}
func (s *twentyPublishSession) PrepareCreate(ctx context.Context, i db.Issue) (jsontext.Value, error) {
	if i.Status != "open" {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenIssueRequired)
	}
	p := twentyCreatePayload{Title: i.Title, Status: s.config.OpenStatus}
	p.Body.Markdown = i.Body
	if err := validateTwentyCreate(p); err != nil {
		return nil, err
	}
	if err := s.validateSchema(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}
func (s *twentyPublishSession) Create(ctx context.Context, raw jsontext.Value, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	var p twentyCreatePayload
	if json.Unmarshal(raw, &p, json.RejectUnknownMembers(true)) != nil || admit == nil || p.Status != s.config.OpenStatus {
		return zero, fmt.Errorf("invalid Twenty creation request")
	}
	if err := validateTwentyCreate(p); err != nil {
		return zero, err
	}
	if err := s.validateSchema(ctx); err != nil {
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
	response, err := s.session.request(ctx, http.MethodPost, "/rest/tasks", encoded)
	if err != nil {
		return zero, fmt.Errorf("twenty creation response was lost")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == 429 || response.StatusCode >= 500 {
		s.session.client.deferRequests(response.Header.Get("Retry-After"), 0)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return zero, fmt.Errorf("twenty creation failed (HTTP %d)", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return zero, fmt.Errorf("twenty creation response was incomplete")
	}
	var envelope struct {
		Data struct {
			Task struct {
				ID string `json:"id"`
			} `json:"createTask"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return zero, fmt.Errorf("invalid Twenty creation response")
	}
	id, err := CanonicalID(envelope.Data.Task.ID)
	if err != nil {
		return zero, fmt.Errorf("twenty creation response has no task identity")
	}
	remote := db.IssuePublicationRemote{ExternalID: "task:" + id}
	observed, err := s.ReadCreated(ctx, remote)
	if err != nil {
		return remote, err
	}
	if observed.Status != "open" {
		return remote, fmt.Errorf("twenty created task is not open")
	}
	return observed, nil
}
func (s *twentyPublishSession) ReadCreated(ctx context.Context, identity db.IssuePublicationRemote) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	id, err := CanonicalID(strings.TrimPrefix(identity.ExternalID, "task:"))
	if err != nil {
		return zero, fmt.Errorf("twenty adoption requires a task UUID")
	}
	obs, err := s.session.ReadStatus(ctx, s.config, id)
	if err != nil {
		return zero, err
	}
	return db.IssuePublicationRemote{ExternalID: "task:" + id, URL: s.config.WebOrigin + "/object/task/" + id, Status: obs.Status, RawStatus: obs.RawStatus, ClosedReason: obs.ClosedReason, Version: obs.Version, SourceUpdatedAt: &obs.Version}, nil
}

var _ issuesync.PublishAdapter = (*Adapter)(nil)
var _ issuesync.PublishSession = (*twentyPublishSession)(nil)
