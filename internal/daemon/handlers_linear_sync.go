package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/linearsync"
)

const issueSyncProviderLinear = "linear"

func linearSyncValidation(message string) error {
	return api.NewError(http.StatusBadRequest, "validation", message, "", nil)
}
func linearSyncEnableRemoteError(err error, message string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return api.NewError(http.StatusGatewayTimeout, "linear_timeout", context.DeadlineExceeded.Error(), "", nil)
	}
	return linearSyncValidation(message)
}
func linearSyncEnableParams(ctx context.Context, cfg ServerConfig, in *api.EnableIssueSyncRequest) (db.UpsertIssueSyncBindingParams, error) {
	var empty db.UpsertIssueSyncBindingParams
	daemonConfig, err := config.NormalizeLinearSyncConfig(cfg.LinearSyncConfig)
	if err != nil {
		return empty, linearSyncValidation(err.Error())
	}
	cfg.LinearSyncConfig = daemonConfig
	stringsIn := map[string]string{}
	prefix := true
	for key, value := range in.Body.Config {
		switch key {
		case "workspace_id", "team_id", "project_id", "since", "closed_state_id", "open_state_id":
			text, ok := value.(string)
			if !ok {
				return empty, linearSyncValidation("Linear " + key + " must be a string")
			}
			stringsIn[key] = text
		case "title_prefix":
			var ok bool
			prefix, ok = value.(bool)
			if !ok {
				return empty, linearSyncValidation("Linear title_prefix must be a boolean")
			}
		default:
			return empty, linearSyncValidation("unknown Linear sync config key")
		}
	}
	resolved := linearsync.Config{WorkspaceID: stringsIn["workspace_id"], TeamID: stringsIn["team_id"], ProjectID: stringsIn["project_id"], Since: stringsIn["since"], TitlePrefix: &prefix, ClosedStateID: stringsIn["closed_state_id"], OpenStateID: stringsIn["open_state_id"]}
	interval := 300
	expected := &db.IssueSyncBindingPrecondition{}
	existing, err := cfg.DB.IssueSyncBindingByProject(ctx, in.ProjectID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return empty, issueSyncStorageError(err, issueSyncProviderLinear)
	}
	if err == nil {
		if existing.Provider != issueSyncProviderLinear {
			return empty, issueSyncStorageError(db.ErrIssueSyncProjectAlreadyBound, issueSyncProviderLinear)
		}
		previous, err := linearsync.DecodeConfig(existing.Config)
		if err != nil {
			return empty, linearSyncValidation("stored Linear config is invalid")
		}
		resolved.StatusSync = previous.StatusSync
		for _, key := range []string{"workspace_id", "team_id", "project_id", "since", "closed_state_id", "open_state_id"} {
			if _, present := stringsIn[key]; !present {
				switch key {
				case "workspace_id":
					resolved.WorkspaceID = previous.WorkspaceID
				case "team_id":
					resolved.TeamID = previous.TeamID
				case "project_id":
					resolved.ProjectID = previous.ProjectID
				case "since":
					resolved.Since = previous.Since
				case "closed_state_id":
					resolved.ClosedStateID = previous.ClosedStateID
				case "open_state_id":
					resolved.OpenStateID = previous.OpenStateID
				}
			}
		}
		if _, present := in.Body.Config["title_prefix"]; !present {
			prefix = previous.UseTitlePrefix()
		}
		interval = existing.IntervalSeconds
		expected = &db.IssueSyncBindingPrecondition{ID: existing.ID, Config: existing.Config, IntervalSeconds: existing.IntervalSeconds}
	}
	resolved.StatusSync, err = issueSyncMode(in.Body.StatusSync, resolved.StatusSync)
	if err != nil {
		return empty, err
	}
	interval, err = issueSyncIntervalSeconds(in.Body, interval, "Linear")
	if err != nil {
		return empty, err
	}
	raw, err := linearsync.EncodeConfig(resolved)
	if err != nil {
		return empty, linearSyncValidation(err.Error())
	}
	resolved, err = linearsync.DecodeConfig(raw)
	if err != nil {
		return empty, linearSyncValidation(err.Error())
	}
	if expected.ID != 0 && (existing.SourceKey != resolved.SourceKey() || existing.RemoteID != resolved.RemoteID()) {
		return empty, linearSyncValidation("Linear source identity is immutable; use another Kata project")
	}
	if err := issueSyncEnableAuthority(ctx, cfg.DB, in.ProjectID, issueSyncProviderLinear); err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	session, err := linearSyncFetcher(cfg).ForRun(ctx, resolved)
	if err != nil {
		return empty, linearSyncEnableRemoteError(err, "Linear credentials are unavailable; configure the daemon token environment variable")
	}
	if resolved.StatusSync == "two-way" {
		if _, ok := session.(linearsync.StatusSession); !ok {
			return empty, linearSyncValidation("Linear session does not support two-way status sync")
		}
	}
	scope, err := session.Scope(ctx, resolved)
	if err != nil {
		return empty, linearSyncEnableRemoteError(err, "cannot access Linear scope")
	}
	states, err := session.States(ctx, resolved)
	if err != nil {
		return empty, linearSyncEnableRemoteError(err, "cannot access Linear states")
	}
	if _, err = linearsync.BuildImportBatch(resolved.SourceKey(), resolved, scope, states, nil); err != nil {
		return empty, linearSyncValidation(err.Error())
	}
	if err = linearsync.ValidateStatusTargets(resolved, states); err != nil {
		return empty, linearSyncValidation(err.Error())
	}
	name := scope.Name
	if strings.TrimSpace(name) == "" {
		name = "Linear team " + resolved.TeamID
	}
	return db.UpsertIssueSyncBindingParams{ProjectID: in.ProjectID, Provider: issueSyncProviderLinear, SourceKey: resolved.SourceKey(), RemoteID: resolved.RemoteID(), DisplayName: name, Config: raw, IntervalSeconds: interval, ExpectedBinding: expected}, nil
}
func linearSyncFetcher(cfg ServerConfig) linearsync.Fetcher {
	if cfg.LinearSyncFetcher != nil {
		return cfg.LinearSyncFetcher
	}
	return linearsync.NewClient(linearsync.ClientConfig{Daemon: cfg.LinearSyncConfig})
}
func linearSyncRunner(cfg ServerConfig) *issuesync.Runner {
	return linearsync.NewRunner(linearsync.RunnerConfig{Store: cfg.DB, Fetcher: linearSyncFetcher(cfg), Progress: cfg.LinearSyncProgress, EventSink: githubSyncEventSink(cfg), Logger: cfg.Logger})
}
