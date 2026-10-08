package twentysync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// OpenStatus verifies workspace identity before independent status scanning.
func (a *Adapter) OpenStatus(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (issuesync.StatusRun, error) {
	if a.fetcher == nil {
		return nil, fmt.Errorf("twenty adapter requires fetcher")
	}
	c, err := bindingConfig(b)
	if err != nil {
		return nil, statusReadError(err)
	}
	session, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, statusReadError(err)
	}
	if _, err := verifyWorkspace(ctx, session, c); err != nil {
		return nil, statusReadError(err)
	}
	status, ok := session.(StatusSession)
	if !ok {
		return nil, nil
	}
	return &twentyStatusRun{status: status, config: c}, nil
}

// twentyStatusRun reuses the session OpenStatus verified; the session's pinned
// token cannot move to another workspace during the run.
type twentyStatusRun struct {
	status StatusSession
	config Config
}

func twentyStatusID(m db.IssueStatusMapping) (string, error) {
	if !strings.HasPrefix(m.Mapping.ExternalID, "task:") {
		return "", blockedStatus("invalid Twenty mapping identity")
	}
	return CanonicalID(strings.TrimPrefix(m.Mapping.ExternalID, "task:"))
}
func (r *twentyStatusRun) ReadStatus(ctx context.Context, m db.IssueStatusMapping) (issuesync.StatusObservation, error) {
	id, err := twentyStatusID(m)
	if err != nil {
		return issuesync.StatusObservation{}, statusReadError(err)
	}
	return r.status.ReadStatus(ctx, r.config, id)
}
func (r *twentyStatusRun) WriteStatus(ctx context.Context, m db.IssueStatusMapping, desired string, admit func() error) (issuesync.StatusObservation, error) {
	id, err := twentyStatusID(m)
	if err != nil {
		return issuesync.StatusObservation{}, statusReadError(err)
	}
	return r.status.WriteStatus(ctx, r.config, id, desired, admit)
}

var _ issuesync.StatusAdapter = (*Adapter)(nil)
