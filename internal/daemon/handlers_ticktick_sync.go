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
	"go.kenn.io/kata/internal/tickticksync"
)

const issueSyncProviderTickTick = "ticktick"

func tickTickSyncValidation(message string) error {
	return api.NewError(http.StatusBadRequest, "validation", message, "", nil)
}
func tickTickSyncEnableRemoteError(err error, message string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return api.NewError(http.StatusGatewayTimeout, "ticktick_timeout", context.DeadlineExceeded.Error(), "", nil)
	}
	return tickTickSyncValidation(message)
}
func tickTickSyncEnableParams(ctx context.Context, cfg ServerConfig, in *api.EnableIssueSyncRequest) (db.UpsertIssueSyncBindingParams, error) {
	var empty db.UpsertIssueSyncBindingParams
	_, err := config.NormalizeTickTickSyncConfig(cfg.TickTickSyncConfig)
	if err != nil {
		return empty, tickTickSyncValidation(err.Error())
	}
	stringsIn := map[string]string{}
	prefix := true
	for key, value := range in.Body.Config {
		switch key {
		case "project_id":
			text, ok := value.(string)
			if !ok {
				return empty, tickTickSyncValidation("TickTick " + key + " must be a string")
			}
			stringsIn[key] = text
		case "title_prefix":
			var ok bool
			prefix, ok = value.(bool)
			if !ok {
				return empty, tickTickSyncValidation("TickTick title_prefix must be a boolean")
			}
		default:
			return empty, tickTickSyncValidation("unknown TickTick sync config key")
		}
	}
	resolved := tickticksync.Config{ProjectID: stringsIn["project_id"], TitlePrefix: &prefix}
	interval := 300
	expected := &db.IssueSyncBindingPrecondition{}
	existing, err := cfg.DB.IssueSyncBindingByProject(ctx, in.ProjectID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return empty, issueSyncStorageError(err, issueSyncProviderTickTick)
	}
	if err == nil {
		if existing.Provider != issueSyncProviderTickTick {
			return empty, issueSyncStorageError(db.ErrIssueSyncProjectAlreadyBound, issueSyncProviderTickTick)
		}
		previous, err := tickticksync.DecodeConfig(existing.Config)
		if err != nil {
			return empty, tickTickSyncValidation("stored TickTick config is invalid")
		}
		resolved.StatusSync = previous.StatusSync
		if _, present := stringsIn["project_id"]; !present {
			resolved.ProjectID = previous.ProjectID
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
	interval, err = issueSyncIntervalSeconds(in.Body, interval, "TickTick")
	if err != nil {
		return empty, err
	}
	raw, err := tickticksync.EncodeConfig(resolved)
	if err != nil {
		return empty, tickTickSyncValidation(err.Error())
	}
	resolved, err = tickticksync.DecodeConfig(raw)
	if err != nil {
		return empty, tickTickSyncValidation(err.Error())
	}
	if expected.ID != 0 && (existing.SourceKey != resolved.SourceKey() || existing.RemoteID != resolved.RemoteID()) {
		return empty, tickTickSyncValidation("TickTick source identity is immutable; use another Kata project")
	}
	if err := issueSyncEnableAuthority(ctx, cfg.DB, in.ProjectID, issueSyncProviderTickTick); err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	session, err := tickTickSyncFetcher(cfg).ForRun(ctx, resolved)
	if err != nil {
		return empty, tickTickSyncEnableRemoteError(err, "TickTick credentials are unavailable; configure the daemon token environment variable")
	}
	if resolved.StatusSync == "two-way" {
		if _, ok := session.(tickticksync.StatusSession); !ok {
			return empty, tickTickSyncValidation("TickTick session does not support two-way status sync")
		}
	}
	project, err := session.Project(ctx)
	if err != nil {
		return empty, tickTickSyncEnableRemoteError(err, "cannot access TickTick project")
	}
	if err = tickticksync.ValidateProject(resolved, project); err != nil {
		return empty, tickTickSyncValidation(err.Error())
	}
	name := project.Name
	if strings.TrimSpace(name) == "" {
		name = "TickTick project " + project.ID
	}
	return db.UpsertIssueSyncBindingParams{ProjectID: in.ProjectID, Provider: issueSyncProviderTickTick, SourceKey: resolved.SourceKey(), RemoteID: resolved.RemoteID(), DisplayName: name, Config: raw, IntervalSeconds: interval, ExpectedBinding: expected}, nil
}
func tickTickSyncFetcher(cfg ServerConfig) tickticksync.Fetcher {
	if cfg.TickTickSyncFetcher != nil {
		return cfg.TickTickSyncFetcher
	}
	return tickticksync.NewClient(tickticksync.ClientConfig{TokenEnv: cfg.TickTickSyncConfig.TokenEnv})
}
func tickTickSyncRunner(cfg ServerConfig) *issuesync.Runner {
	return tickticksync.NewRunner(tickticksync.RunnerConfig{Store: cfg.DB, Fetcher: tickTickSyncFetcher(cfg), Progress: cfg.TickTickSyncProgress, EventSink: githubSyncEventSink(cfg), Logger: cfg.Logger})
}
