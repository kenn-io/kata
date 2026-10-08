package linearsync

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// OpenStatus opens a scoped status session for durable reconciliation.
func (a *Adapter) OpenStatus(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (issuesync.StatusRun, error) {
	if a.fetcher == nil {
		return nil, blockedStatus("linear adapter requires fetcher")
	}
	c, err := bindingConfig(b)
	if err != nil {
		return nil, statusReadError(err)
	}
	s, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, statusReadError(err)
	}
	if _, err := verifyScope(ctx, s, c); err != nil {
		return nil, statusReadError(err)
	}
	status, ok := s.(StatusSession)
	if !ok {
		return nil, blockedStatus("Linear session does not support two-way status")
	}
	return &linearStatusRun{session: s, status: status, config: c, adapter: a, bindingID: b.ID}, nil
}

type linearStatusRun struct {
	session   Session
	status    StatusSession
	config    Config
	cache     statusSchema
	adapter   *Adapter
	bindingID int64
}

func mappingID(m db.IssueStatusMapping) (string, error) {
	if !strings.HasPrefix(m.Mapping.ExternalID, "issue:") {
		return "", blockedStatus("invalid Linear mapping identity")
	}
	id, err := CanonicalID(strings.TrimPrefix(m.Mapping.ExternalID, "issue:"))
	return id, statusReadError(err)
}
func (r *linearStatusRun) ReadStatus(ctx context.Context, m db.IssueStatusMapping) (issuesync.StatusObservation, error) {
	id, err := mappingID(m)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	if r.adapter != nil {
		if obs, ok := r.adapter.takeObservation(r.bindingID, r.config.SourceKey(), id, m.State.Observed); ok {
			return obs, nil
		}
	}
	if s, ok := r.session.(*clientSession); ok {
		obs, _, err := s.readStatusCached(ctx, r.config, id, &r.cache)
		return obs, err
	}
	return r.status.ReadStatus(ctx, r.config, id)
}
func (r *linearStatusRun) WriteStatus(ctx context.Context, m db.IssueStatusMapping, desired string, admit func() error) (issuesync.StatusObservation, error) {
	id, err := mappingID(m)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	return r.status.WriteStatus(ctx, r.config, id, desired, admit)
}

var _ issuesync.StatusAdapter = (*Adapter)(nil)
