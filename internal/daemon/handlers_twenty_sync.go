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
	"go.kenn.io/kata/internal/twentysync"
)

const issueSyncProviderTwenty = "twenty"

func twentySyncValidation(message string) error {
	return api.NewError(http.StatusBadRequest, "validation", message, "", nil)
}
func twentySyncEnableRemoteError(err error, message string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return api.NewError(http.StatusGatewayTimeout, "twenty_timeout", context.DeadlineExceeded.Error(), "", nil)
	}
	return twentySyncValidation(message)
}

func twentySyncEnableParams(ctx context.Context, cfg ServerConfig, in *api.EnableIssueSyncRequest) (db.UpsertIssueSyncBindingParams, error) {
	var empty db.UpsertIssueSyncBindingParams
	daemonConfig, err := config.NormalizeTwentySyncConfig(cfg.TwentySyncConfig)
	if err != nil {
		return empty, twentySyncValidation(err.Error())
	}
	resolved := twentysync.Config{APIOrigin: daemonConfig.APIOrigin, WebOrigin: daemonConfig.WebOrigin}
	interval := 300
	expected := &db.IssueSyncBindingPrecondition{}
	existing, err := cfg.DB.IssueSyncBindingByProject(ctx, in.ProjectID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return empty, issueSyncStorageError(err, issueSyncProviderTwenty)
	}
	if err == nil {
		if existing.Provider != issueSyncProviderTwenty {
			return empty, issueSyncStorageError(db.ErrIssueSyncProjectAlreadyBound, issueSyncProviderTwenty)
		}
		previous, err := twentysync.DecodeConfig(existing.Config)
		if err != nil {
			return empty, twentySyncValidation("stored Twenty config is invalid")
		}
		resolved = previous
		resolved.APIOrigin = daemonConfig.APIOrigin
		resolved.WebOrigin = daemonConfig.WebOrigin
		if resolved.SourceKey() != existing.SourceKey || resolved.RemoteID() != existing.RemoteID {
			return empty, twentySyncValidation("Twenty source identity is immutable; use another Kata project")
		}
		interval = existing.IntervalSeconds
		expected = &db.IssueSyncBindingPrecondition{ID: existing.ID, Config: existing.Config, IntervalSeconds: existing.IntervalSeconds}
	}
	if err := applyTwentySyncSettings(&resolved, in.Body.Config); err != nil {
		return empty, err
	}
	resolved.StatusSync, err = issueSyncMode(in.Body.StatusSync, resolved.StatusSync)
	if err != nil {
		return empty, err
	}
	interval, err = issueSyncIntervalSeconds(in.Body, interval, "Twenty")
	if err != nil {
		return empty, err
	}
	resolved, err = twentysync.NormalizeDiscoveryConfig(resolved)
	if err != nil {
		return empty, twentySyncValidation(err.Error())
	}
	if err := issueSyncEnableAuthority(ctx, cfg.DB, in.ProjectID, issueSyncProviderTwenty); err != nil {
		return empty, err
	}
	resolved, name, err := verifyTwentySyncSource(ctx, cfg, resolved)
	if err != nil {
		return empty, err
	}
	raw, err := twentysync.EncodeConfig(resolved)
	if err != nil {
		return empty, twentySyncValidation(err.Error())
	}
	return db.UpsertIssueSyncBindingParams{ProjectID: in.ProjectID, Provider: issueSyncProviderTwenty, SourceKey: resolved.SourceKey(), RemoteID: resolved.RemoteID(), DisplayName: name, Config: raw, IntervalSeconds: interval, ExpectedBinding: expected}, nil
}

// applyTwentySyncSettings overlays request settings; omitted keys keep saved values.
func applyTwentySyncSettings(resolved *twentysync.Config, settings map[string]any) error {
	for key, value := range settings {
		switch key {
		case "since":
			text, ok := value.(string)
			if !ok {
				return twentySyncValidation("Twenty since must be a string")
			}
			resolved.Since = text
		case "closed_status", "open_status":
			text, ok := value.(string)
			if !ok {
				return twentySyncValidation("Twenty " + key + " must be a string")
			}
			if strings.TrimSpace(text) == "" {
				return twentySyncValidation("Twenty status options must be nonempty API values")
			}
			if key == "closed_status" {
				resolved.ClosedStatus = text
			} else {
				resolved.OpenStatus = text
			}
		case "title_prefix":
			prefix, ok := value.(bool)
			if !ok {
				return twentySyncValidation("Twenty title_prefix must be a boolean")
			}
			resolved.TitlePrefix = &prefix
		case "open_statuses":
			values, err := twentySyncOpenStatuses(value)
			if err != nil {
				return err
			}
			resolved.OpenStatuses = values
		default:
			return twentySyncValidation("unknown Twenty sync config key")
		}
	}
	return nil
}

func twentySyncOpenStatuses(value any) ([]string, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, twentySyncValidation("Twenty open_statuses must be an array of strings")
	}
	if len(list) == 0 {
		return nil, twentySyncValidation("Twenty open_statuses requires at least one value")
	}
	values := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, twentySyncValidation("Twenty open_statuses must be an array of strings")
		}
		values = append(values, text)
	}
	return values, nil
}

// verifyTwentySyncSource discovers the API key's workspace, verifies it again
// with a bound session, and checks the live task schema. It returns the bound
// config and the workspace display name.
func verifyTwentySyncSource(ctx context.Context, cfg ServerConfig, resolved twentysync.Config) (twentysync.Config, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	session, err := twentySyncFetcher(cfg).ForRun(ctx, resolved)
	if err != nil {
		return resolved, "", twentySyncEnableRemoteError(err, "Twenty credentials are unavailable; configure the daemon token environment variable")
	}
	workspace, err := session.Workspace(ctx, resolved)
	if err != nil {
		return resolved, "", twentySyncEnableRemoteError(err, "cannot access Twenty workspace")
	}
	id, err := twentysync.CanonicalID(workspace.ID)
	if err != nil {
		return resolved, "", twentySyncValidation("invalid Twenty workspace identity")
	}
	if resolved.WorkspaceID != "" && resolved.WorkspaceID != id {
		return resolved, "", twentySyncValidation("Twenty API key workspace differs from the saved binding")
	}
	resolved.WorkspaceID = id
	// A discovery session cannot read tasks or schema. Reopen with the identity
	// we just verified, then verify it again with that session's pinned token.
	session, err = twentySyncFetcher(cfg).ForRun(ctx, resolved)
	if err != nil {
		return resolved, "", twentySyncEnableRemoteError(err, "Twenty credentials are unavailable")
	}
	verified, err := session.Workspace(ctx, resolved)
	if err != nil {
		return resolved, "", twentySyncEnableRemoteError(err, "cannot access Twenty workspace")
	}
	if verified.ID != id {
		return resolved, "", twentySyncValidation("Twenty workspace changed during validation")
	}
	if resolved.StatusSync == "two-way" {
		if _, ok := session.(twentysync.StatusSession); !ok {
			return resolved, "", twentySyncValidation("Twenty session does not support two-way status sync")
		}
	}
	schema, err := session.Schema(ctx, resolved)
	if err != nil {
		return resolved, "", twentySyncEnableRemoteError(err, "cannot access Twenty task status schema; check API permissions and status mappings")
	}
	if _, err := twentysync.BuildImportBatch(resolved.SourceKey(), resolved, schema, nil); err != nil {
		return resolved, "", twentySyncValidation(err.Error())
	}
	name := verified.DisplayName
	if strings.TrimSpace(name) == "" {
		name = "Twenty workspace " + id
	}
	return resolved, name, nil
}

func twentySyncFetcher(cfg ServerConfig) twentysync.Fetcher {
	if cfg.TwentySyncFetcher != nil {
		return cfg.TwentySyncFetcher
	}
	return twentysync.NewClient(twentysync.ClientConfig{Daemon: cfg.TwentySyncConfig})
}
func twentySyncRunner(cfg ServerConfig) *issuesync.Runner {
	return twentysync.NewRunner(twentysync.RunnerConfig{Store: cfg.DB, Fetcher: twentySyncFetcher(cfg), Progress: cfg.TwentySyncProgress, EventSink: githubSyncEventSink(cfg), Logger: cfg.Logger})
}
