package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/todoistsync"
)

const issueSyncProviderTodoist = "todoist"

func todoistSyncValidation(message string) error {
	return api.NewError(http.StatusBadRequest, "validation", message, "", nil)
}
func todoistSyncRemoteError(err error, message string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return api.NewError(http.StatusGatewayTimeout, "todoist_timeout", context.DeadlineExceeded.Error(), "", nil)
	}
	return todoistSyncValidation(message)
}
func todoistSyncEnableParams(ctx context.Context, cfg ServerConfig, in *api.EnableIssueSyncRequest) (db.UpsertIssueSyncBindingParams, error) {
	var empty db.UpsertIssueSyncBindingParams
	daemonConfig, err := config.NormalizeTodoistSyncConfig(cfg.TodoistSyncConfig)
	if err != nil {
		return empty, todoistSyncValidation(err.Error())
	}
	request, err := parseTodoistSyncRequest(in.Body.Config)
	if err != nil {
		return empty, err
	}
	interval := 300
	expected := &db.IssueSyncBindingPrecondition{}
	var previous *todoistsync.Config
	existing, err := cfg.DB.IssueSyncBindingByProject(ctx, in.ProjectID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return empty, issueSyncStorageError(err, issueSyncProviderTodoist)
	}
	if err == nil {
		if existing.Provider != issueSyncProviderTodoist {
			return empty, issueSyncStorageError(db.ErrIssueSyncProjectAlreadyBound, issueSyncProviderTodoist)
		}
		saved, err := todoistsync.DecodeConfig(existing.Config)
		if err != nil {
			return empty, todoistSyncValidation("stored Todoist config is invalid")
		}
		previous = &saved
		interval = existing.IntervalSeconds
		expected = &db.IssueSyncBindingPrecondition{ID: existing.ID, Config: existing.Config, IntervalSeconds: existing.IntervalSeconds}
	}
	c, err := todoistSyncConfig(daemonConfig.APIOrigin, request, previous, time.Now())
	if err != nil {
		return empty, err
	}
	c.StatusSync, err = issueSyncMode(in.Body.StatusSync, c.StatusSync)
	if err != nil {
		return empty, err
	}
	interval, err = issueSyncIntervalSeconds(in.Body, interval, "Todoist")
	if err != nil {
		return empty, err
	}
	if err := issueSyncEnableAuthority(ctx, cfg.DB, in.ProjectID, issueSyncProviderTodoist); err != nil {
		return empty, err
	}
	raw, name, err := todoistSyncVerifySource(ctx, todoistSyncFetcher(cfg), c)
	if err != nil {
		return empty, err
	}
	c, err = todoistsync.DecodeConfig(raw)
	if err != nil {
		return empty, todoistSyncValidation(err.Error())
	}
	return db.UpsertIssueSyncBindingParams{ProjectID: in.ProjectID, Provider: issueSyncProviderTodoist, SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: name, Config: raw, IntervalSeconds: interval, ExpectedBinding: expected}, nil
}

// todoistSyncRequest holds the enable options a client may select.
type todoistSyncRequest struct {
	projectID, historySince *string
	titlePrefix             *bool
}

func parseTodoistSyncRequest(raw map[string]any) (todoistSyncRequest, error) {
	var r todoistSyncRequest
	for key, value := range raw {
		switch key {
		case "project_id", "history_since":
			text, ok := value.(string)
			if !ok {
				return r, todoistSyncValidation("Todoist " + key + " must be a string")
			}
			if key == "project_id" {
				r.projectID = &text
			} else {
				r.historySince = &text
			}
		case "title_prefix":
			prefix, ok := value.(bool)
			if !ok {
				return r, todoistSyncValidation("Todoist title_prefix must be a boolean")
			}
			r.titlePrefix = &prefix
		default:
			return r, todoistSyncValidation("unknown Todoist sync config key")
		}
	}
	return r, nil
}

// todoistSyncConfig merges a request over the saved config. Omitted options keep
// saved values; source identity and the history floor are immutable.
func todoistSyncConfig(origin string, r todoistSyncRequest, previous *todoistsync.Config, now time.Time) (todoistsync.Config, error) {
	c := todoistsync.Config{APIOrigin: origin, TitlePrefix: new(true)}
	if previous != nil {
		c = *previous
		c.APIOrigin = origin
		c.TitlePrefix = new(previous.UseTitlePrefix())
	}
	if r.projectID != nil {
		c.ProjectID = *r.projectID
	}
	if r.titlePrefix != nil {
		c.TitlePrefix = r.titlePrefix
	}
	if r.historySince != nil {
		if *r.historySince == "" {
			return c, todoistSyncValidation("Todoist history_since cannot be empty")
		}
		c.HistorySince = *r.historySince
	}
	if c.HistorySince == "" {
		c.HistorySince = now.UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	}
	cutoff, err := todoistsync.ParseHistorySince(c.HistorySince)
	if err != nil {
		return c, todoistSyncValidation(err.Error())
	}
	if cutoff.After(now) {
		return c, todoistSyncValidation("Todoist history_since cannot be in the future")
	}
	c.HistorySince = cutoff.Format(time.RFC3339)
	if previous != nil && (c.APIOrigin != previous.APIOrigin || c.ProjectID != previous.ProjectID || c.HistorySince != previous.HistorySince) {
		return c, todoistSyncValidation("Todoist source identity and history floor are immutable; use another Kata project")
	}
	if err := todoistsync.ValidateID(c.ProjectID); err != nil {
		return c, todoistSyncValidation(err.Error())
	}
	return c, nil
}

// todoistSyncVerifySource pins the credential account and checks the selected
// project before the binding is saved. It returns the encoded config and name.
func todoistSyncVerifySource(ctx context.Context, fetcher todoistsync.Fetcher, c todoistsync.Config) (jsontext.Value, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	account, err := fetcher.Account(ctx)
	if err != nil {
		return nil, "", todoistSyncRemoteError(err, "Todoist credentials are unavailable; configure the daemon token environment variable")
	}
	if c.AccountID != "" && c.AccountID != account {
		return nil, "", todoistSyncValidation("Todoist credential account differs from the saved binding")
	}
	c.AccountID = account
	raw, err := todoistsync.EncodeConfig(c)
	if err != nil {
		return nil, "", todoistSyncValidation(err.Error())
	}
	c, err = todoistsync.DecodeConfig(raw)
	if err != nil {
		return nil, "", todoistSyncValidation(err.Error())
	}
	session, err := fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, "", todoistSyncRemoteError(err, "cannot validate Todoist account")
	}
	if c.StatusSync == "two-way" {
		if _, ok := session.(todoistsync.StatusSession); !ok {
			return nil, "", todoistSyncValidation("Todoist session does not support two-way status sync")
		}
	}
	project, err := session.Project(ctx, c)
	if err != nil {
		return nil, "", todoistSyncRemoteError(err, "cannot access the selected active Todoist project")
	}
	name := project.Name
	if strings.TrimSpace(name) == "" {
		name = "Todoist project " + c.ProjectID
	}
	return raw, name, nil
}
func todoistSyncFetcher(cfg ServerConfig) todoistsync.Fetcher {
	if cfg.TodoistSyncFetcher != nil {
		return cfg.TodoistSyncFetcher
	}
	return todoistsync.NewClient(todoistsync.ClientConfig{Daemon: cfg.TodoistSyncConfig})
}
func todoistSyncRunner(cfg ServerConfig) *issuesync.Runner {
	return todoistsync.NewRunner(todoistsync.RunnerConfig{Store: cfg.DB, Fetcher: todoistSyncFetcher(cfg), Progress: cfg.TodoistSyncProgress, EventSink: githubSyncEventSink(cfg), Logger: cfg.Logger})
}
