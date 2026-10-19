package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/twentysync"
)

func registerIssuePublishHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "publishIssue", Method: "POST", Path: "/api/v1/projects/{project_id}/issue-sync/{provider}/publish"}, func(ctx context.Context, in *api.PublishIssueRequest) (*api.PublishIssueResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		provider, err := validateIssueSyncProvider(in.Provider)
		if err != nil {
			return nil, err
		}
		b := in.Body
		if (b.Adopt != "" && b.ResolveNotCreated) || (b.ResolveNotCreated && (strings.TrimSpace(b.Attempt) == "" || strings.TrimSpace(b.Message) == "")) || (!b.ResolveNotCreated && (b.Attempt != "" || b.Message != "")) {
			return nil, api.NewError(http.StatusBadRequest, "invalid_publish_request", "use --adopt or --resolve-not-created; resolution requires --attempt and --message", "", nil)
		}
		actor, err := attributedActor(ctx, b.Actor)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		if err := issueSyncEnableAuthority(ctx, cfg.DB, in.ProjectID, provider); err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, b.IssueRef, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		publisher := issuesync.NewPublisher(issuesync.PublisherConfig{Store: cfg.DB, Adapter: issuePublishAdapter(cfg, provider), EventSink: githubSyncEventSink(cfg)})
		result, err := publisher.Publish(ctx, issuesync.PublishRequest{ProjectID: in.ProjectID, IssueID: issue.ID, Provider: provider, Actor: actor, Adopt: b.Adopt, ResolveNotCreated: b.ResolveNotCreated, AttemptUID: b.Attempt, Message: b.Message})
		if err != nil {
			return nil, issuePublishError(err, provider)
		}
		return &api.PublishIssueResponse{Body: api.PublishIssueResponseBody{Provider: result.Provider, IssueUID: result.IssueUID, IssueRef: result.IssueRef, SourceKey: result.SourceKey, ExternalID: result.ExternalID, URL: result.URL, Result: result.Result}}, nil
	})
}
func issuePublishAdapter(cfg ServerConfig, provider string) issuesync.PublishAdapter {
	switch provider {
	case "github":
		return githubsync.NewPublishAdapter(githubSyncFetcher(cfg))
	case "notion":
		return notionsync.NewAdapter(cfg.DB, notionSyncFetcher(cfg))
	case "linear":
		return linearsync.NewAdapter(cfg.DB, linearSyncFetcher(cfg))
	case "plane":
		return planesync.NewAdapter(cfg.DB, planeSyncFetcher(cfg))
	case "ticktick":
		return tickticksync.NewAdapter(cfg.DB, tickTickSyncFetcher(cfg))
	case "todoist":
		return todoistsync.NewAdapter(cfg.DB, todoistSyncFetcher(cfg))
	case "twenty":
		return twentysync.NewAdapter(cfg.DB, twentySyncFetcher(cfg))
	default:
		return nil
	}
}
func issuePublishError(err error, provider string) error {
	if pending, ok := errors.AsType[*db.IssuePublicationPendingError](err); ok {
		return issuePublicationPendingError(pending)
	}
	if publication, ok := errors.AsType[*issuesync.PublishError](err); ok {
		if message, hint, known := publication.ValidationDetails(); known {
			return api.NewError(http.StatusBadRequest, publication.Code, message, hint, nil)
		}
		status := http.StatusConflict
		if publication.Code == "invalid_publish_request" {
			status = http.StatusBadRequest
		}
		return api.NewError(status, publication.Code, strings.ReplaceAll(publication.Code, "_", " "), "Check the binding and provider access; inspect sync status before retrying.", nil)
	}
	if errors.Is(err, db.ErrIssueSyncAlreadyRunning) {
		return issueSyncRunError(err)
	}
	return issueSyncStorageError(err, provider)
}
func issuePublicationPendingError(p *db.IssuePublicationPendingError) error {
	return api.NewError(http.StatusConflict, "issue_publication_pending", p.Error(), "Inspect the provider before choosing adoption or resolving not created.", map[string]any{"attempt_uid": p.AttemptUID, "provider": p.Provider, "issue_uid": p.IssueUID, "phase": p.Phase})
}
