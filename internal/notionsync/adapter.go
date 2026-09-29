package notionsync

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

const runTimeout = 25 * time.Minute

// Adapter prepares a complete Notion batch before the shared engine imports it.
type Adapter struct {
	store   db.Storage
	fetcher Fetcher
}

var _ issuesync.Adapter = (*Adapter)(nil)

// NewAdapter connects daemon-owned reads to guarded issue-sync preparation.
func NewAdapter(store db.Storage, fetcher Fetcher) *Adapter {
	return &Adapter{store: store, fetcher: fetcher}
}

// Provider identifies the built-in Notion adapter.
func (*Adapter) Provider() string { return "notion" }

// InitialPhase describes schema validation before page reads.
func (*Adapter) InitialPhase() string { return "source" }

// Prepare validates the source and prepares a complete bounded import batch.
func (a *Adapter) Prepare(ctx context.Context, binding db.IssueSyncBinding, startedAt time.Time) (issuesync.Prepared, error) {
	prepared := issuesync.Prepared{Binding: binding}
	if a.store == nil || a.fetcher == nil {
		return prepared, fmt.Errorf("notion adapter requires store and fetcher")
	}
	if binding.Provider != "notion" {
		return prepared, fmt.Errorf("notion adapter requires a Notion binding")
	}
	config, err := DecodeConfig(binding.Config)
	if err != nil {
		return prepared, err
	}
	if binding.RemoteID != config.DataSourceID || binding.SourceKey != "notion:"+config.DataSourceID {
		return prepared, fmt.Errorf("notion binding source identity does not match config")
	}
	session, err := a.fetcher.ForRun(ctx)
	if err != nil {
		return prepared, err
	}
	source, err := session.DataSource(ctx, config.DataSourceID)
	if err != nil {
		return prepared, err
	}
	if err := ValidateSchema(config, source); err != nil {
		return prepared, err
	}
	databaseID, err := canonicalID(source.DatabaseID)
	if err != nil {
		return prepared, err
	}
	database, err := session.Database(ctx, databaseID)
	if err != nil {
		return prepared, err
	}
	if database.ID != databaseID {
		return prepared, fmt.Errorf("notion parent database identity does not match")
	}
	found := false
	for _, child := range database.DataSources {
		if child.ID == config.DataSourceID {
			found = true
		}
	}
	if !found {
		return prepared, fmt.Errorf("notion parent database no longer contains the data source")
	}
	config.DatabaseID = databaseID
	raw, err := EncodeConfig(config)
	if err != nil {
		return prepared, err
	}
	displayName := SourceDisplayName(source)
	if binding.DisplayName != displayName || string(binding.Config) != string(raw) {
		binding, err = a.store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{BindingID: binding.ID, DisplayName: displayName, Config: raw, StartedAt: &startedAt})
		if err != nil {
			return prepared, err
		}
		prepared.Binding = binding
	}
	cutoff, err := ParseSince(config.Since)
	if err != nil {
		return prepared, err
	}
	var since *time.Time
	if binding.LastCursorAt != nil {
		overlap := binding.LastCursorAt.Add(-2 * time.Minute)
		since = &overlap
	}
	if cutoff != nil && (since == nil || cutoff.After(*since)) {
		since = cutoff
	}
	issuesync.ReportProgress(ctx, "pages", 0, 0)
	pages, err := session.Pages(ctx, config, since)
	if err != nil {
		return prepared, err
	}
	eligible := make([]Page, 0, len(pages))
	statuses := map[string]bool{}
	for _, property := range source.Properties {
		if property.ID == config.StatusPropertyID {
			for _, option := range property.Options {
				statuses[option.ID] = true
			}
		}
	}
	for _, page := range pages {
		if page.IsArchived || page.InTrash || page.DataSourceID != config.DataSourceID || (cutoff != nil && !page.UpdatedAt.After(*cutoff)) {
			continue
		}
		if page.StatusID != nil && !statuses[*page.StatusID] {
			return prepared, fmt.Errorf("notion page %s: status ID is absent from the source schema", page.ID)
		}
		eligible = append(eligible, page)
	}
	issuesync.ReportProgress(ctx, "pages", len(pages), 0)
	issuesync.ReportProgress(ctx, "content", 0, len(eligible))
	batch := db.ImportBatchParams{ProjectID: binding.ProjectID, Source: binding.SourceKey, Actor: "notion-sync", ReconcileLabelsForUnchanged: map[string][]string{}, Items: make([]db.ImportItem, 0, len(eligible))}
	serializedBytes := 0
	for _, page := range eligible {
		content, err := session.Content(ctx, config, page)
		if err != nil {
			return prepared, fmt.Errorf("notion page %s: %w", page.ID, err)
		}
		if content.Page.StatusID != nil && !statuses[*content.Page.StatusID] {
			return prepared, fmt.Errorf("notion page %s: status ID is absent from the source schema", page.ID)
		}
		mapped, err := BuildImportBatch(binding.SourceKey, config, []PageContent{content})
		if err != nil {
			return prepared, fmt.Errorf("notion page %s: %w", page.ID, err)
		}
		item := mapped.Items[0]
		raw, err := json.Marshal(item)
		if err != nil {
			return prepared, fmt.Errorf("cannot serialize Notion import item")
		}
		serializedBytes += len(raw)
		if serializedBytes > maxImportItemBytes {
			return prepared, fmt.Errorf("notion serialized import items exceed 64 MiB")
		}
		batch.ReconcileLabelsForUnchanged[item.ExternalID] = mapped.ReconcileLabelsForUnchanged[item.ExternalID]
		batch.Items = append(batch.Items, item)
		issuesync.ReportProgress(ctx, "content", len(batch.Items), len(eligible))
	}
	if err := db.ValidateImportBatch(batch); err != nil {
		return prepared, fmt.Errorf("invalid Notion import batch")
	}
	prepared.Batch = batch
	return prepared, nil
}

// RunnerConfig supplies daemon-owned dependencies to the shared issue runner.
type RunnerConfig struct {
	Store          db.Storage
	Fetcher        Fetcher
	Progress       *issuesync.ProgressTracker
	Clock          func() time.Time
	Logger         *slog.Logger
	Interval       time.Duration
	Wake           <-chan struct{}
	EventSink      func(context.Context, int64, []db.Event) error
	EventSinkFrom  func(context.Context, int64, []db.Event, activity.Admission) error
	DrainAdmission activity.WaitableAdmission
}

// NewRunner applies the fixed Notion claim, deadline, and import chunk limits.
func NewRunner(c RunnerConfig) *issuesync.Runner {
	return issuesync.NewRunner(issuesync.RunnerConfig{
		Store:            c.Store,
		Adapter:          NewAdapter(c.Store, c.Fetcher),
		Progress:         c.Progress,
		Clock:            c.Clock,
		Logger:           c.Logger,
		Interval:         c.Interval,
		Wake:             c.Wake,
		EventSink:        c.EventSink,
		EventSinkFrom:    c.EventSinkFrom,
		DrainAdmission:   c.DrainAdmission,
		RunTimeout:       runTimeout,
		StaleLockTTL:     30 * time.Minute,
		InitialBatchSize: 5000,
	})
}
