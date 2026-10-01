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
	"go.kenn.io/kata/internal/planesync"
)

const issueSyncProviderPlane = "plane"

func planeSyncValidation(message string) error {
	return api.NewError(http.StatusBadRequest, "validation", message, "", nil)
}
func planeSyncEnableRemoteError(err error, message string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return api.NewError(http.StatusGatewayTimeout, "plane_timeout", context.DeadlineExceeded.Error(), "", nil)
	}
	return planeSyncValidation(message)
}
func planeSyncEnableParams(ctx context.Context, cfg ServerConfig, in *api.EnableIssueSyncRequest) (db.UpsertIssueSyncBindingParams, error) {
	var empty db.UpsertIssueSyncBindingParams
	daemonConfig, err := config.NormalizePlaneSyncConfig(cfg.PlaneSyncConfig)
	if err != nil {
		return empty, planeSyncValidation(err.Error())
	}
	stringsIn := map[string]string{}
	prefix := true
	for key, value := range in.Body.Config {
		switch key {
		case "workspace", "project_id", "since", "closed_state_id", "open_state_id":
			text, ok := value.(string)
			if !ok {
				return empty, planeSyncValidation("Plane " + key + " must be a string")
			}
			stringsIn[key] = text
		case "title_prefix":
			var ok bool
			prefix, ok = value.(bool)
			if !ok {
				return empty, planeSyncValidation("Plane title_prefix must be a boolean")
			}
		default:
			return empty, planeSyncValidation("unknown Plane sync config key")
		}
	}
	resolved := planesync.Config{APIOrigin: daemonConfig.APIOrigin, WebOrigin: daemonConfig.WebOrigin, Workspace: stringsIn["workspace"], ProjectID: stringsIn["project_id"], Since: stringsIn["since"], TitlePrefix: &prefix, ClosedStateID: stringsIn["closed_state_id"], OpenStateID: stringsIn["open_state_id"]}
	interval := 300
	expected := &db.IssueSyncBindingPrecondition{}
	existing, err := cfg.DB.IssueSyncBindingByProject(ctx, in.ProjectID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return empty, issueSyncStorageError(err, issueSyncProviderPlane)
	}
	if err == nil {
		if existing.Provider != issueSyncProviderPlane {
			return empty, issueSyncStorageError(db.ErrIssueSyncProjectAlreadyBound, issueSyncProviderPlane)
		}
		previous, err := planesync.DecodeConfig(existing.Config)
		if err != nil {
			return empty, planeSyncValidation("stored Plane config is invalid")
		}
		resolved.StatusSync = previous.StatusSync
		for _, key := range []string{"workspace", "project_id", "since", "closed_state_id", "open_state_id"} {
			if _, present := stringsIn[key]; !present {
				switch key {
				case "workspace":
					resolved.Workspace = previous.Workspace
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
	interval, err = issueSyncIntervalSeconds(in.Body, interval, "Plane")
	if err != nil {
		return empty, err
	}
	raw, err := planesync.EncodeConfig(resolved)
	if err != nil {
		return empty, planeSyncValidation(err.Error())
	}
	resolved, err = planesync.DecodeConfig(raw)
	if err != nil {
		return empty, planeSyncValidation(err.Error())
	}
	if expected.ID != 0 && (existing.SourceKey != resolved.SourceKey() || existing.RemoteID != resolved.RemoteID()) {
		return empty, planeSyncValidation("Plane source identity is immutable; use another Kata project")
	}
	if err := issueSyncEnableAuthority(ctx, cfg.DB, in.ProjectID, issueSyncProviderPlane); err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	session, err := planeSyncFetcher(cfg).ForRun(ctx, resolved)
	if err != nil {
		return empty, planeSyncEnableRemoteError(err, "Plane credentials are unavailable; configure the daemon token environment variable")
	}
	if resolved.StatusSync == "two-way" {
		if _, ok := session.(planesync.StatusSession); !ok {
			return empty, planeSyncValidation("Plane session does not support two-way status sync")
		}
	}
	project, err := session.Project(ctx, resolved)
	if err != nil {
		return empty, planeSyncEnableRemoteError(err, "cannot access Plane project")
	}
	states, err := session.States(ctx, resolved)
	if err != nil {
		return empty, planeSyncEnableRemoteError(err, "cannot access Plane states")
	}
	if _, err = planesync.BuildImportBatch(resolved.SourceKey(), resolved, project, states, nil); err != nil {
		return empty, planeSyncValidation(err.Error())
	}
	if err = planesync.ValidateStatusTargets(resolved, states); err != nil {
		return empty, planeSyncValidation(err.Error())
	}
	name := project.Name
	if strings.TrimSpace(name) == "" {
		name = project.Identifier
	}
	if strings.TrimSpace(name) == "" {
		name = "Plane project " + project.ID
	}
	return db.UpsertIssueSyncBindingParams{ProjectID: in.ProjectID, Provider: issueSyncProviderPlane, SourceKey: resolved.SourceKey(), RemoteID: resolved.RemoteID(), DisplayName: name, Config: raw, IntervalSeconds: interval, ExpectedBinding: expected}, nil
}
func planeSyncFetcher(cfg ServerConfig) planesync.Fetcher {
	if cfg.PlaneSyncFetcher != nil {
		return cfg.PlaneSyncFetcher
	}
	return planesync.NewClient(planesync.ClientConfig{Daemon: cfg.PlaneSyncConfig})
}
func planeSyncRunner(cfg ServerConfig) *issuesync.Runner {
	return planesync.NewRunner(planesync.RunnerConfig{Store: cfg.DB, Fetcher: planeSyncFetcher(cfg), Progress: cfg.PlaneSyncProgress, EventSink: githubSyncEventSink(cfg), Logger: cfg.Logger})
}
