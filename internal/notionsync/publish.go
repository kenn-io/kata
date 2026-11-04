package notionsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// OpenPublish captures a provider session for the configured source.
func (a *Adapter) OpenPublish(ctx context.Context, b db.IssueSyncBinding) (issuesync.PublishSession, error) {
	if a.fetcher == nil {
		return nil, fmt.Errorf("notion publishing requires a fetcher")
	}
	cfg, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	if b.Provider != "notion" || b.RemoteID != cfg.DataSourceID || b.SourceKey != "notion:"+cfg.DataSourceID {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	raw, err := a.fetcher.ForRun(ctx)
	if err != nil {
		return nil, err
	}
	session, ok := raw.(*clientSession)
	if !ok {
		return nil, fmt.Errorf("notion session does not support publishing")
	}
	return &notionPublishSession{session: session, config: cfg}, nil
}

type notionPublishSession struct {
	session *clientSession
	config  Config
}
type notionCreateText struct {
	Type string `json:"type"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
}
type notionCreateStatus struct {
	ID string `json:"id"`
}
type notionCreateProperty struct {
	Title  []notionCreateText  `json:"title,omitempty"`
	Status *notionCreateStatus `json:"status,omitempty"`
}
type notionCreatePayload struct {
	Parent struct {
		DataSourceID string `json:"data_source_id"`
	} `json:"parent"`
	Properties map[string]notionCreateProperty `json:"properties"`
	Markdown   string                          `json:"markdown"`
}

const maxNotionCreateBytes = 500 * 1024

func (s *notionPublishSession) source(ctx context.Context) (Config, StatusSchema, error) {
	source, err := s.session.DataSource(ctx, s.config.DataSourceID)
	if err != nil {
		return Config{}, StatusSchema{}, err
	}
	if err := ValidateSchema(s.config, source); err != nil {
		return Config{}, StatusSchema{}, issuesync.NewPublicationValidationError(issuesync.PublicationSchemaInvalid)
	}
	schema, err := ResolveStatusSchema(s.config, source)
	if err != nil {
		return Config{}, StatusSchema{}, issuesync.NewPublicationValidationError(issuesync.PublicationSchemaInvalid)
	}
	database, err := s.session.Database(ctx, source.DatabaseID)
	if err != nil {
		return Config{}, StatusSchema{}, err
	}
	for _, child := range database.DataSources {
		if child.ID == s.config.DataSourceID {
			cfg := s.config
			cfg.DatabaseID = database.ID
			return cfg, schema, nil
		}
	}
	return Config{}, StatusSchema{}, issuesync.NewPublicationValidationError(issuesync.PublicationDestinationUnavailable)
}
func (s *notionPublishSession) PrepareCreate(ctx context.Context, issue db.Issue) (jsontext.Value, error) {
	if issue.Status != "open" {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenIssueRequired)
	}
	if strings.TrimSpace(issue.Title) == "" || !utf8.ValidString(issue.Title) || !utf8.ValidString(issue.Body) {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationNotionContentInvalid)
	}
	cfg, schema, err := s.source(ctx)
	if err != nil {
		return nil, err
	}
	target, err := schema.Target("open")
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationNotionOpenTargetRequired)
	}
	title := []rune(issue.Title)
	if len(title) > 200000 {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationNotionContentInvalid)
	}
	var parts []notionCreateText
	for len(title) > 0 {
		n := min(len(title), 2000)
		part := notionCreateText{Type: "text"}
		part.Text.Content = string(title[:n])
		parts = append(parts, part)
		title = title[n:]
	}
	payload := notionCreatePayload{Properties: map[string]notionCreateProperty{cfg.TitlePropertyID: {Title: parts}, cfg.StatusPropertyID: {Status: &notionCreateStatus{ID: target}}}, Markdown: issue.Body}
	payload.Parent.DataSourceID = cfg.DataSourceID
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxNotionCreateBytes {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationNotionContentInvalid)
	}
	return raw, nil
}
func (s *notionPublishSession) Create(ctx context.Context, raw jsontext.Value, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	if admit == nil || len(raw) > maxNotionCreateBytes {
		return zero, fmt.Errorf("invalid Notion creation request or missing admission")
	}
	var payload notionCreatePayload
	if err := json.Unmarshal(raw, &payload, json.RejectUnknownMembers(true)); err != nil {
		return zero, fmt.Errorf("invalid Notion creation payload")
	}
	cfg, schema, err := s.source(ctx)
	if err != nil {
		return zero, err
	}
	title, hasTitle := payload.Properties[cfg.TitlePropertyID]
	status, hasStatus := payload.Properties[cfg.StatusPropertyID]
	if payload.Parent.DataSourceID != cfg.DataSourceID || len(payload.Properties) != 2 || !hasTitle || !hasStatus || status.Status == nil || len(status.Title) != 0 || title.Status != nil || len(title.Title) == 0 || len(title.Title) > 100 {
		return zero, fmt.Errorf("notion creation payload does not match the selected source")
	}
	for _, part := range title.Title {
		if part.Type != "text" || utf8.RuneCountInString(part.Text.Content) > 2000 {
			return zero, fmt.Errorf("invalid Notion title text")
		}
	}
	state, err := schema.Classify(&status.Status.ID)
	if err != nil || state != "open" {
		return zero, fmt.Errorf("notion creation status must remain open")
	}
	var wire wirePage
	if err := s.session.requestWithAdmission(ctx, http.MethodPost, "/v1/pages", payload, &wire, func() error { return admit(ctx) }); err != nil {
		return zero, err
	}
	return notionPublicationObservation(cfg, schema, wire, "")
}
func (s *notionPublishSession) ReadCreated(ctx context.Context, identity db.IssuePublicationRemote) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	id, err := canonicalID(strings.TrimPrefix(identity.ExternalID, "page:"))
	if err != nil {
		return zero, fmt.Errorf("notion adoption requires a page ID")
	}
	cfg, schema, err := s.session.statusSchema(ctx, s.config)
	if err != nil {
		return zero, err
	}
	var wire wirePage
	if err := s.session.request(ctx, http.MethodGet, "/v1/pages/"+id, nil, &wire); err != nil {
		return zero, err
	}
	return notionPublicationObservation(cfg, schema, wire, id)
}
func notionPublicationObservation(cfg Config, schema StatusSchema, wire wirePage, expectedID string) (db.IssuePublicationRemote, error) {
	var remote db.IssuePublicationRemote
	id, err := canonicalID(wire.ID)
	if err != nil || wire.Object != "page" || (expectedID != "" && id != expectedID) {
		return remote, fmt.Errorf("invalid Notion publication identity")
	}
	source, sourceErr := canonicalID(wire.Parent.DataSourceID)
	database, databaseErr := canonicalID(wire.Parent.DatabaseID)
	if wire.Parent.Type != "data_source_id" || sourceErr != nil || databaseErr != nil || source != cfg.DataSourceID || database != cfg.DatabaseID {
		return remote, fmt.Errorf("notion publication page belongs to a different source")
	}
	remote.ExternalID = "page:" + id
	remote.Locator = id
	page, available, err := wire.observation(cfg)
	if err != nil {
		return remote, err
	}
	if !available {
		return remote, fmt.Errorf("notion publication page is unavailable")
	}
	state, err := schema.Classify(page.StatusID)
	if err != nil {
		return remote, err
	}
	remote.URL = page.URL
	remote.Status = state
	remote.RawStatus = page.StatusID
	remote.Version = page.UpdatedAt
	remote.SourceUpdatedAt = &remote.Version
	if state == "closed" {
		remote.ClosedReason = "done"
	}
	return remote, nil
}

var _ issuesync.PublishAdapter = (*Adapter)(nil)
var _ issuesync.PublishSession = (*notionPublishSession)(nil)
