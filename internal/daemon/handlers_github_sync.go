package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
)

const defaultGitHubSyncIntervalSeconds = 300
const issueSyncProviderGitHub = "github"

func registerIssueSyncHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "enableIssueSync",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issue-sync/{provider}/enable",
	}, func(ctx context.Context, in *api.EnableIssueSyncRequest) (*api.IssueSyncResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		provider, err := validateIssueSyncProvider(in.Provider)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		params, err := issueSyncEnableParams(ctx, cfg, provider, in)
		if err != nil {
			return nil, err
		}
		binding, err := cfg.DB.UpsertIssueSyncBinding(ctx, params)
		if err != nil {
			return nil, issueSyncStorageError(err)
		}
		if cfg.GitHubSyncWake != nil {
			cfg.GitHubSyncWake()
		}
		status, err := cfg.DB.IssueSyncStatusByProject(ctx, in.ProjectID)
		if err != nil {
			return nil, issueSyncStorageError(err)
		}
		body, err := issueSyncBody(&binding, status)
		if err != nil {
			return nil, err
		}
		return &api.IssueSyncResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "disableIssueSync",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issue-sync/{provider}/disable",
	}, func(ctx context.Context, in *api.DisableIssueSyncRequest) (*api.IssueSyncResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		provider, err := validateIssueSyncProvider(in.Provider)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		if _, err := issueSyncBindingByProjectProvider(ctx, cfg.DB, in.ProjectID, provider); err != nil {
			return nil, issueSyncStorageError(err)
		}
		binding, err := cfg.DB.DisableIssueSyncBinding(ctx, in.ProjectID)
		if err != nil {
			return nil, issueSyncStorageError(err)
		}
		status, err := cfg.DB.IssueSyncStatusByProject(ctx, in.ProjectID)
		if err != nil {
			return nil, issueSyncStorageError(err)
		}
		body, err := issueSyncBody(&binding, status)
		if err != nil {
			return nil, err
		}
		return &api.IssueSyncResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "getIssueSyncStatus",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/issue-sync/{provider}/status",
	}, func(ctx context.Context, in *api.IssueSyncStatusRequest) (*api.IssueSyncResponse, error) {
		provider, err := validateIssueSyncProvider(in.Provider)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		binding, err := issueSyncBindingByProjectProvider(ctx, cfg.DB, in.ProjectID, provider)
		if errors.Is(err, db.ErrNotFound) {
			return &api.IssueSyncResponse{Body: api.IssueSyncBody{
				Status: api.IssueSyncStatusOut{
					ProjectID: in.ProjectID,
					Provider:  provider,
					Enabled:   false,
					State:     "not_enabled",
				},
			}}, nil
		}
		if err != nil {
			return nil, issueSyncStorageError(err)
		}
		status, err := cfg.DB.IssueSyncStatusByProject(ctx, in.ProjectID)
		if err != nil {
			return nil, issueSyncStorageError(err)
		}
		body, err := issueSyncBody(&binding, status)
		if err != nil {
			return nil, err
		}
		return &api.IssueSyncResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "runIssueSyncOnce",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issue-sync/{provider}/once",
	}, func(ctx context.Context, in *api.RunIssueSyncOnceRequest) (*api.RunIssueSyncOnceResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		provider, err := validateIssueSyncProvider(in.Provider)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		binding, err := issueSyncBindingByProjectProvider(ctx, cfg.DB, in.ProjectID, provider)
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(http.StatusBadRequest, "validation", "issue sync is not enabled for this project", "", nil)
		}
		if err != nil {
			return nil, issueSyncStorageError(err)
		}
		if !binding.Enabled {
			return nil, api.NewError(http.StatusBadRequest, "validation", "issue sync is disabled for this project", "", nil)
		}
		runner := githubSyncRunner(cfg)
		result, err := runner.RunOnce(ctx, binding.ID)
		if err != nil {
			return nil, issueSyncRunError(err)
		}
		bindingOut, err := issueSyncBindingOut(result.Binding)
		if err != nil {
			return nil, err
		}
		return &api.RunIssueSyncOnceResponse{Body: api.RunIssueSyncOnceResponseBody{
			Binding: ptr(bindingOut),
			Status:  issueSyncStatusOut(result.Status, result.Binding.Provider, result.Binding.Enabled),
			Import:  result.Import,
		}}, nil
	})
}

func validateIssueSyncProvider(provider string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return "", api.NewError(http.StatusBadRequest, "validation", "issue sync provider is required", "", nil)
	}
	if provider != issueSyncProviderGitHub {
		return "", api.NewError(http.StatusBadRequest, "validation", fmt.Sprintf("issue sync provider %q is not supported", provider), "", nil)
	}
	return provider, nil
}

func issueSyncEnableParams(ctx context.Context, cfg ServerConfig, provider string, in *api.EnableIssueSyncRequest) (db.UpsertIssueSyncBindingParams, error) {
	switch provider {
	case issueSyncProviderGitHub:
		return githubSyncEnableParams(ctx, cfg, in)
	default:
		return db.UpsertIssueSyncBindingParams{}, api.NewError(http.StatusBadRequest, "validation", fmt.Sprintf("issue sync provider %q is not supported", provider), "", nil)
	}
}

func githubSyncEnableParams(ctx context.Context, cfg ServerConfig, in *api.EnableIssueSyncRequest) (db.UpsertIssueSyncBindingParams, error) {
	host := strings.ToLower(strings.TrimSpace(issueSyncConfigString(in.Body.Config, "host")))
	if host == "" {
		host = "github.com"
	}
	owner := strings.TrimSpace(issueSyncConfigString(in.Body.Config, "owner"))
	repoName := strings.TrimSpace(issueSyncConfigString(in.Body.Config, "repo"))
	if owner == "" || repoName == "" {
		return db.UpsertIssueSyncBindingParams{}, api.NewError(http.StatusBadRequest, "validation", "GitHub owner and repo are required", "", nil)
	}
	intervalSeconds, err := githubSyncIntervalSeconds(in.Body.IntervalSeconds, in.Body.Interval)
	if err != nil {
		return db.UpsertIssueSyncBindingParams{}, err
	}
	titlePrefix, err := issueSyncConfigBool(in.Body.Config, "title_prefix", true)
	if err != nil {
		return db.UpsertIssueSyncBindingParams{}, err
	}
	fetched, err := githubSyncFetcher(cfg).Repository(ctx, host, owner, repoName)
	if err != nil {
		return db.UpsertIssueSyncBindingParams{}, api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
	}
	if strings.TrimSpace(fetched.NodeID) == "" {
		return db.UpsertIssueSyncBindingParams{}, api.NewError(http.StatusBadRequest, "validation", "GitHub repository node_id is required", "", nil)
	}
	if strings.TrimSpace(fetched.FullName) != "" {
		canonicalOwner, canonicalRepo, ok := strings.Cut(fetched.FullName, "/")
		if !ok || strings.TrimSpace(canonicalOwner) == "" || strings.TrimSpace(canonicalRepo) == "" {
			return db.UpsertIssueSyncBindingParams{}, api.NewError(http.StatusBadRequest, "validation", "GitHub repository full_name is invalid", "", nil)
		}
		owner = canonicalOwner
		repoName = canonicalRepo
	}
	ghConfig := githubsync.Config{
		Host:        host,
		Owner:       owner,
		Repo:        repoName,
		RepoID:      fetched.ID,
		TitlePrefix: &titlePrefix,
	}
	configJSON, err := githubsync.EncodeConfig(ghConfig)
	if err != nil {
		return db.UpsertIssueSyncBindingParams{}, api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
	}
	return db.UpsertIssueSyncBindingParams{
		ProjectID:       in.ProjectID,
		Provider:        issueSyncProviderGitHub,
		SourceKey:       "github:" + fetched.NodeID,
		RemoteID:        fetched.NodeID,
		DisplayName:     ghConfig.DisplayName(),
		Config:          configJSON,
		IntervalSeconds: intervalSeconds,
	}, nil
}

func issueSyncConfigString(config map[string]any, key string) string {
	if config == nil {
		return ""
	}
	switch v := config[key].(type) {
	case string:
		return v
	default:
		return ""
	}
}

func issueSyncConfigBool(config map[string]any, key string, defaultValue bool) (bool, error) {
	if config == nil {
		return defaultValue, nil
	}
	v, ok := config[key]
	if !ok || v == nil {
		return defaultValue, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, api.NewError(http.StatusBadRequest, "validation", "GitHub sync title_prefix must be a boolean", "", nil)
	}
	return b, nil
}

func githubSyncIntervalSeconds(intervalSeconds int, interval string) (int, error) {
	if strings.TrimSpace(interval) == "" {
		if intervalSeconds > 0 {
			return intervalSeconds, nil
		}
		return defaultGitHubSyncIntervalSeconds, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(interval))
	if err != nil {
		seconds, atoiErr := strconv.Atoi(strings.TrimSpace(interval))
		if atoiErr != nil {
			return 0, api.NewError(http.StatusBadRequest, "validation", fmt.Sprintf("invalid GitHub sync interval %q", interval), "", nil)
		}
		d = time.Duration(seconds) * time.Second
	}
	if d <= 0 {
		return defaultGitHubSyncIntervalSeconds, nil
	}
	return int(d.Round(time.Second) / time.Second), nil
}

func githubSyncFetcher(cfg ServerConfig) githubsync.Fetcher {
	if cfg.GitHubSyncFetcher != nil {
		return cfg.GitHubSyncFetcher
	}
	factory := cfg.GitHubSyncFetcherFactory
	if factory == nil {
		factory = defaultGitHubSyncHTTPFetcher
	}
	return factory(cfg.GitHubSyncConfig)
}

func defaultGitHubSyncHTTPFetcher(cfg config.GitHubSyncConfig) githubsync.Fetcher {
	return githubsync.NewHTTPFetcher(githubsync.HTTPFetcherConfig{
		CredentialResolver: githubsync.NewCredentialResolver(cfg, nil),
	})
}

func githubSyncRunner(cfg ServerConfig) GitHubSyncRunner {
	factory := cfg.GitHubSyncRunnerFactory
	if factory == nil {
		factory = NewDefaultGitHubSyncRunner
	}
	return factory(GitHubSyncRunnerConfig{
		Store:     cfg.DB,
		Fetcher:   githubSyncFetcher(cfg),
		EventSink: githubSyncEventSink(cfg),
		Logger:    cfg.Logger,
	})
}

func githubSyncEventSink(cfg ServerConfig) func(context.Context, int64, []db.Event) error {
	return func(_ context.Context, projectID int64, events []db.Event) error {
		for i := range events {
			evt := events[i]
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: projectID})
			cfg.Hooks.Enqueue(evt)
		}
		return nil
	}
}

func issueSyncBindingByProjectProvider(ctx context.Context, store db.Storage, projectID int64, provider string) (db.IssueSyncBinding, error) {
	binding, err := store.IssueSyncBindingByProject(ctx, projectID)
	if err != nil {
		return db.IssueSyncBinding{}, err
	}
	if binding.Provider != provider {
		return db.IssueSyncBinding{}, db.ErrNotFound
	}
	return binding, nil
}

func issueSyncStorageError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return api.NewError(http.StatusNotFound, "issue_sync_not_found", "issue sync binding not found", "", nil)
	}
	if errors.Is(err, db.ErrIssueSyncNotEnabled) {
		return api.NewError(http.StatusBadRequest, "validation", "issue sync is not enabled for this project", "", nil)
	}
	if errors.Is(err, db.ErrIssueSyncFederationBinding) {
		return api.NewError(http.StatusConflict, "issue_sync_federation_conflict",
			"project is a federation spoke; enable GitHub sync on the hub project so federation can replicate GitHub issues to spokes", "", nil)
	}
	if errors.Is(err, db.ErrIssueSyncProjectAlreadyBound) || errors.Is(err, db.ErrImportValidation) {
		return api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
	}
	return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
}

func issueSyncRunError(err error) error {
	if errors.Is(err, db.ErrIssueSyncAlreadyRunning) {
		return api.NewError(http.StatusConflict, "issue_sync_already_running", "issue sync is already running for this project", "", nil)
	}
	if errors.Is(err, db.ErrNotFound) || errors.Is(err, db.ErrIssueSyncNotEnabled) {
		return api.NewError(http.StatusBadRequest, "validation", "issue sync is not enabled for this project", "", nil)
	}
	if errors.Is(err, db.ErrImportValidation) {
		return api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
	}
	return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
}

func issueSyncBody(binding *db.IssueSyncBinding, status db.IssueSyncStatus) (api.IssueSyncBody, error) {
	bindingOut, err := issueSyncBindingOut(*binding)
	if err != nil {
		return api.IssueSyncBody{}, err
	}
	return api.IssueSyncBody{
		Binding: ptr(bindingOut),
		Status:  issueSyncStatusOut(status, binding.Provider, binding.Enabled),
	}, nil
}

func issueSyncBindingOut(binding db.IssueSyncBinding) (api.IssueSyncBindingOut, error) {
	config, err := api.DecodeJSONMap(binding.Config)
	if err != nil {
		return api.IssueSyncBindingOut{}, api.NewError(http.StatusInternalServerError, "internal", "issue sync binding config is invalid", "", nil)
	}
	return api.IssueSyncBindingOut{
		ID:              binding.ID,
		ProjectID:       binding.ProjectID,
		Provider:        binding.Provider,
		SourceKey:       binding.SourceKey,
		RemoteID:        binding.RemoteID,
		DisplayName:     binding.DisplayName,
		Config:          config,
		Enabled:         binding.Enabled,
		IntervalSeconds: binding.IntervalSeconds,
		LastCursorAt:    binding.LastCursorAt,
		CreatedAt:       binding.CreatedAt,
		UpdatedAt:       binding.UpdatedAt,
	}, nil
}

func issueSyncStatusOut(status db.IssueSyncStatus, provider string, enabled bool) api.IssueSyncStatusOut {
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	return api.IssueSyncStatusOut{
		BindingID:     status.BindingID,
		ProjectID:     status.ProjectID,
		Provider:      provider,
		Enabled:       enabled,
		State:         state,
		SyncStartedAt: status.SyncStartedAt,
		LastAttemptAt: status.LastAttemptAt,
		LastSuccessAt: status.LastSuccessAt,
		LastErrorAt:   status.LastErrorAt,
		LastError:     status.LastError,
		LastCreated:   status.LastCreated,
		LastUpdated:   status.LastUpdated,
		LastUnchanged: status.LastUnchanged,
		LastComments:  status.LastComments,
	}
}

func ptr[T any](v T) *T {
	return &v
}
