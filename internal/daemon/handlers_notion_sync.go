package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/notionsync"
)

const issueSyncProviderNotion = "notion"

func notionSyncValidation(message string) error {
	return api.NewError(http.StatusBadRequest, "validation", message, "", nil)
}

func notionSyncEnableRemoteError(err error, validationMessage string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return api.NewError(http.StatusGatewayTimeout, "notion_timeout", context.DeadlineExceeded.Error(), "", nil)
	}
	return notionSyncValidation(validationMessage)
}

func notionSyncEnableParams(ctx context.Context, cfg ServerConfig, in *api.EnableIssueSyncRequest) (db.UpsertIssueSyncBindingParams, error) {
	var empty db.UpsertIssueSyncBindingParams
	stringsIn := map[string]string{}
	selectors := notionsync.Selectors{}
	titlePrefix := true
	for key, value := range in.Body.Config {
		switch key {
		case "data_source_id", "database", "status_property", "assignee_property", "since":
			text, ok := value.(string)
			if !ok {
				return empty, notionSyncValidation("Notion " + key + " must be a string")
			}
			stringsIn[key] = text
		case "title_prefix":
			var ok bool
			titlePrefix, ok = value.(bool)
			if !ok {
				return empty, notionSyncValidation("Notion title_prefix must be a boolean")
			}
		case "done_statuses":
			// JSONMap decodes arrays as []any; direct embedding callers may supply []string.
			switch values := value.(type) {
			case []string:
				selectors.DoneStatuses = append([]string{}, values...)
			case []any:
				for _, item := range values {
					text, ok := item.(string)
					if !ok {
						return empty, notionSyncValidation("Notion done_statuses must be a string array")
					}
					selectors.DoneStatuses = append(selectors.DoneStatuses, text)
				}
			default:
				return empty, notionSyncValidation("Notion done_statuses must be a string array")
			}
			if len(selectors.DoneStatuses) == 0 {
				return empty, notionSyncValidation("Notion done_statuses requires at least one selection")
			}
		default:
			return empty, notionSyncValidation("unknown Notion sync config key")
		}
	}
	existing, err := cfg.DB.IssueSyncBindingByProject(ctx, in.ProjectID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return empty, issueSyncStorageError(err, issueSyncProviderNotion)
	}
	expected := &db.IssueSyncBindingPrecondition{}
	var previous *notionsync.Config
	interval := 300
	since := stringsIn["since"]
	if err == nil {
		if existing.Provider != issueSyncProviderNotion {
			return empty, issueSyncStorageError(db.ErrIssueSyncProjectAlreadyBound, issueSyncProviderNotion)
		}
		decoded, err := notionsync.DecodeConfig(existing.Config)
		if err != nil {
			return empty, notionSyncValidation("stored Notion config is invalid")
		}
		previous = &decoded
		if _, present := in.Body.Config["title_prefix"]; !present {
			titlePrefix = decoded.UseTitlePrefix()
		}
		expected = &db.IssueSyncBindingPrecondition{ID: existing.ID, Config: existing.Config, IntervalSeconds: existing.IntervalSeconds}
		interval = existing.IntervalSeconds
		if _, present := stringsIn["since"]; !present {
			since = decoded.Since
		}
		if _, present := stringsIn["status_property"]; !present {
			stringsIn["status_property"] = decoded.StatusPropertyID
		}
		if _, present := stringsIn["assignee_property"]; !present {
			stringsIn["assignee_property"] = decoded.AssigneePropertyID
		}
		if _, present := in.Body.Config["done_statuses"]; !present {
			selectors.DoneStatuses = decoded.DoneStatusIDs
		}
	}
	interval, err = issueSyncIntervalSeconds(in.Body, interval, "Notion")
	if err != nil {
		return empty, err
	}
	if _, err = notionsync.ParseSince(since); err != nil {
		return empty, notionSyncValidation(err.Error())
	}
	source, hasSource := stringsIn["data_source_id"]
	database, hasDatabase := stringsIn["database"]
	if hasSource && hasDatabase {
		return empty, notionSyncValidation("provide exactly one Notion data_source_id or database")
	}
	if !hasSource && !hasDatabase {
		if previous == nil {
			return empty, notionSyncValidation("provide exactly one Notion data_source_id or database")
		}
		source = previous.DataSourceID
	}
	if !hasDatabase {
		if strings.ContainsAny(source, "/:") {
			return empty, notionSyncValidation("Notion data_source_id must be a UUID")
		}
		source, err = notionsync.ParseDatabaseLocator(source)
	} else {
		database, err = notionsync.ParseDatabaseLocator(database)
	}
	if err != nil {
		return empty, notionSyncValidation(err.Error())
	}
	// Preflight avoids upstream work; the upsert transaction rechecks authority.
	if err := issueSyncEnableAuthority(ctx, cfg.DB, in.ProjectID, issueSyncProviderNotion); err != nil {
		return empty, err
	}
	session, err := notionSyncFetcher(cfg).ForRun(ctx)
	if err != nil {
		return empty, notionSyncEnableRemoteError(err, "Notion credentials are unavailable; configure the daemon token environment variable")
	}
	if hasDatabase {
		container, err := session.Database(ctx, database)
		if err != nil {
			return empty, notionSyncEnableRemoteError(err, "cannot access Notion database")
		}
		if container.ID != database {
			return empty, notionSyncValidation("Notion database identity does not match")
		}
		if len(container.DataSources) != 1 {
			choices := make([]string, 0, len(container.DataSources))
			for _, child := range container.DataSources {
				choices = append(choices, fmt.Sprintf("%s (%s)", child.Name, child.ID))
			}
			return empty, notionSyncValidation("Notion database must contain exactly one data source; use data_source_id to select from: " + strings.Join(choices, ", "))
		}
		source = container.DataSources[0].ID
	}
	schema, err := session.DataSource(ctx, source)
	if err != nil {
		return empty, notionSyncEnableRemoteError(err, "cannot access Notion data source schema")
	}
	selectors.StatusProperty = stringsIn["status_property"]
	selectors.AssigneeProperty = stringsIn["assignee_property"]
	resolved, err := notionsync.ResolveConfig(schema, selectors, since)
	if err != nil {
		return empty, notionSyncValidation(err.Error())
	}
	resolved.TitlePrefix = &titlePrefix
	if resolved.DataSourceID != source {
		return empty, notionSyncValidation("Notion data source identity does not match")
	}
	if hasDatabase && resolved.DatabaseID != database {
		return empty, notionSyncValidation("Notion data source parent does not match")
	}
	parent, err := session.Database(ctx, resolved.DatabaseID)
	if err != nil {
		return empty, notionSyncEnableRemoteError(err, "cannot access Notion parent database")
	}
	found := false
	for _, child := range parent.DataSources {
		if child.ID == resolved.DataSourceID {
			found = true
		}
	}
	if parent.ID != resolved.DatabaseID || !found {
		return empty, notionSyncValidation("Notion parent database does not contain the data source")
	}
	if previous != nil {
		if err := notionsync.ValidateReenable(*previous, resolved); err != nil {
			return empty, notionSyncValidation(err.Error())
		}
	}
	raw, err := notionsync.EncodeConfig(resolved)
	if err != nil {
		return empty, notionSyncValidation(err.Error())
	}
	return db.UpsertIssueSyncBindingParams{
		ProjectID: in.ProjectID, Provider: issueSyncProviderNotion, SourceKey: "notion:" + resolved.DataSourceID, RemoteID: resolved.DataSourceID,
		DisplayName: notionsync.SourceDisplayName(schema), Config: raw, IntervalSeconds: interval, ExpectedBinding: expected,
	}, nil
}

func notionSyncFetcher(cfg ServerConfig) notionsync.Fetcher {
	if cfg.NotionSyncFetcher != nil {
		return cfg.NotionSyncFetcher
	}
	return notionsync.NewClient(notionsync.ClientConfig{TokenEnv: cfg.NotionSyncConfig.TokenEnv})
}

func notionSyncRunner(cfg ServerConfig) *issuesync.Runner {
	return notionsync.NewRunner(notionsync.RunnerConfig{Store: cfg.DB, Fetcher: notionSyncFetcher(cfg), Progress: cfg.NotionSyncProgress, EventSink: githubSyncEventSink(cfg), Logger: cfg.Logger})
}
