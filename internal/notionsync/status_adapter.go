package notionsync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// OpenStatus deliberately skips title/assignee/content schema validation.
// Each phase captures its own daemon-owned credential and stays on Notion's
// fixed API origin; an unrelated content error cannot prevent status delivery.
func (a *Adapter) OpenStatus(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (issuesync.StatusRun, error) {
	if a.fetcher == nil {
		return nil, fmt.Errorf("notion adapter requires fetcher")
	}
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, err
	}
	if b.Provider != "notion" || b.RemoteID != c.DataSourceID || b.SourceKey != "notion:"+c.DataSourceID {
		return nil, fmt.Errorf("notion binding source identity does not match config")
	}
	session, err := a.fetcher.ForRun(ctx)
	if err != nil {
		return nil, err
	}
	status, ok := session.(StatusSession)
	if !ok {
		return nil, nil
	}
	return &notionStatusRun{session: status, config: c}, nil
}

type notionStatusRun struct {
	session     StatusSession
	config      Config
	schemaCache statusSchemaCache
}

func (r *notionStatusRun) ReadStatus(ctx context.Context, m db.IssueStatusMapping) (issuesync.StatusObservation, error) {
	if session, ok := r.session.(*clientSession); ok {
		observed, _, err := session.readStatusCached(ctx, r.config, strings.TrimPrefix(m.Mapping.ExternalID, "page:"), &r.schemaCache)
		return observed, err
	}
	return r.session.ReadStatus(ctx, r.config, strings.TrimPrefix(m.Mapping.ExternalID, "page:"))
}
func (r *notionStatusRun) WriteStatus(ctx context.Context, m db.IssueStatusMapping, desired string, admit func() error) (issuesync.StatusObservation, error) {
	return r.session.WriteStatus(ctx, r.config, strings.TrimPrefix(m.Mapping.ExternalID, "page:"), desired, admit)
}

var _ issuesync.StatusAdapter = (*Adapter)(nil)
