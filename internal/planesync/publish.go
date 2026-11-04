package planesync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"html"
	"net/http"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

type planePublishSession struct {
	session *clientSession
	config  Config
}
type planeCreatePayload struct {
	Name            string `json:"name"`
	DescriptionHTML string `json:"description_html"`
	State           string `json:"state"`
}

// OpenPublish captures a provider session for the configured source.
func (a *Adapter) OpenPublish(ctx context.Context, b db.IssueSyncBinding) (issuesync.PublishSession, error) {
	if a.fetcher == nil {
		return nil, fmt.Errorf("plane publishing requires a fetcher")
	}
	cfg, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	if b.Provider != "plane" || b.SourceKey != cfg.SourceKey() || b.RemoteID != cfg.RemoteID() {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	raw, err := a.fetcher.ForRun(ctx, cfg)
	if err != nil {
		return nil, err
	}
	session, ok := raw.(*clientSession)
	if !ok {
		return nil, fmt.Errorf("plane session does not support publishing")
	}
	return &planePublishSession{session: session, config: cfg}, nil
}
func (s *planePublishSession) PrepareCreate(ctx context.Context, issue db.Issue) (jsontext.Value, error) {
	if issue.Status != "open" {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenIssueRequired)
	}
	if !utf8.ValidString(issue.Body) || strings.ContainsRune(issue.Body, '\x00') {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationPlaneContentInvalid)
	}
	body := ""
	if issue.Body != "" {
		body = "<pre>" + html.EscapeString(issue.Body) + "</pre>"
	}
	payload := planeCreatePayload{Name: issue.Title, DescriptionHTML: body}
	if err := validatePlaneCreate(payload); err != nil {
		return nil, err
	}
	if _, err := s.session.Project(ctx, s.config); err != nil {
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
	payload.State, err = schema.target(s.config, "open")
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenTargetRequired)
	}
	return json.Marshal(payload)
}
func validatePlaneCreate(p planeCreatePayload) error {
	if strings.TrimSpace(p.Name) == "" || !utf8.ValidString(p.Name) || utf8.RuneCountInString(p.Name) > 255 || strings.ContainsRune(p.Name, '\x00') || !utf8.ValidString(p.DescriptionHTML) || strings.ContainsRune(p.DescriptionHTML, '\x00') || len(p.DescriptionHTML) > maxDescriptionBytes {
		return issuesync.NewPublicationValidationError(issuesync.PublicationPlaneContentInvalid)
	}
	return nil
}
func (s *planePublishSession) Create(ctx context.Context, raw jsontext.Value, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	var payload planeCreatePayload
	if json.Unmarshal(raw, &payload, json.RejectUnknownMembers(true)) != nil || admit == nil {
		return zero, fmt.Errorf("invalid Plane creation request")
	}
	if err := validatePlaneCreate(payload); err != nil {
		return zero, err
	}
	schema, err := s.session.statusSchema(ctx, s.config)
	if err != nil {
		return zero, err
	}
	if schema.groups[payload.State] != "unstarted" {
		return zero, fmt.Errorf("plane creation state is no longer open")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return zero, err
	}
	response, err := s.session.mutate(ctx, http.MethodPost, s.session.path()+"work-items/", encoded, func() error { return admit(ctx) })
	if err != nil {
		return zero, err
	}
	var identity struct {
		ID      string `json:"id"`
		Project string `json:"project"`
	}
	if json.Unmarshal(response, &identity) != nil {
		return zero, fmt.Errorf("invalid Plane creation response")
	}
	id, idErr := CanonicalID(identity.ID)
	project, projectErr := CanonicalID(identity.Project)
	if idErr != nil || projectErr != nil || project != s.config.ProjectID {
		return zero, fmt.Errorf("plane creation response belongs to a different project")
	}
	remote := db.IssuePublicationRemote{ExternalID: "work-item:" + id}
	observed, err := s.ReadCreated(ctx, remote)
	if err != nil {
		return remote, err
	}
	return observed, nil
}
func (s *planePublishSession) ReadCreated(ctx context.Context, identity db.IssuePublicationRemote) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	id, err := CanonicalID(strings.TrimPrefix(identity.ExternalID, "work-item:"))
	if err != nil {
		return zero, fmt.Errorf("plane adoption requires a work item UUID")
	}
	obs, err := s.session.ReadStatus(ctx, s.config, id)
	if err != nil {
		return zero, err
	}
	remote := db.IssuePublicationRemote{ExternalID: "work-item:" + id, Status: obs.Status, RawStatus: obs.RawStatus, ClosedReason: obs.ClosedReason, Version: obs.Version, SourceUpdatedAt: &obs.Version}
	remote.URL = s.config.WebOrigin + "/" + s.config.Workspace + "/projects/" + s.config.ProjectID + "/issues/" + id + "/"
	return remote, nil
}

var _ issuesync.PublishAdapter = (*Adapter)(nil)
var _ issuesync.PublishSession = (*planePublishSession)(nil)
