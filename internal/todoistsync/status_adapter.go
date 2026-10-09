package todoistsync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// OpenStatus reads workflow metadata independently of content conversion and
// import cutoffs. Todoist's opaque task ID is already part of external_id.
func (a *Adapter) OpenStatus(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (issuesync.StatusRun, error) {
	if a.fetcher == nil {
		return nil, fmt.Errorf("todoist adapter requires fetcher")
	}
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, err
	}
	if b.Provider != "todoist" || b.SourceKey != c.SourceKey() || b.RemoteID != c.RemoteID() {
		return nil, fmt.Errorf("todoist binding source identity does not match config")
	}
	session, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, err
	}
	// Status delivery runs before the content import's project check, so an
	// archived or deleted project must stop it here.
	if _, err := session.Project(ctx, c); err != nil {
		return nil, err
	}
	status, ok := session.(StatusSession)
	if !ok {
		return nil, nil
	}
	return &todoistStatusRun{session: status, config: c}, nil
}

type todoistStatusRun struct {
	session StatusSession
	config  Config
}

func todoistStatusID(m db.IssueStatusMapping) (string, error) {
	if !strings.HasPrefix(m.Mapping.ExternalID, "task:") {
		return "", blocked("invalid Todoist mapping identity")
	}
	id := strings.TrimPrefix(m.Mapping.ExternalID, "task:")
	if err := ValidateID(id); err != nil {
		return "", blocked("invalid Todoist mapping identity")
	}
	return id, nil
}
func (r *todoistStatusRun) ReadStatus(ctx context.Context, m db.IssueStatusMapping) (issuesync.StatusObservation, error) {
	id, err := todoistStatusID(m)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	return r.session.ReadStatus(ctx, r.config, StatusTarget{ID: id, Prior: m.State.Observed})
}
func (r *todoistStatusRun) WriteStatus(ctx context.Context, m db.IssueStatusMapping, desired string, admit func() error) (issuesync.StatusObservation, error) {
	id, err := todoistStatusID(m)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	return r.session.WriteStatus(ctx, r.config, StatusTarget{ID: id, Prior: m.State.Observed}, desired, admit)
}

var _ issuesync.StatusAdapter = (*Adapter)(nil)
