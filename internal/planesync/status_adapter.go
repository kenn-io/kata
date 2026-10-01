package planesync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// OpenStatus reads workflow metadata independently of content conversion and
// import cutoffs. Plane's API UUID is already part of external_id.
func (a *Adapter) OpenStatus(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (issuesync.StatusRun, error) {
	if a.fetcher == nil {
		return nil, fmt.Errorf("plane adapter requires fetcher")
	}
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, err
	}
	if b.Provider != "plane" || b.SourceKey != c.SourceKey() || b.RemoteID != c.RemoteID() {
		return nil, fmt.Errorf("plane binding source identity does not match config")
	}
	session, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return nil, err
	}
	status, ok := session.(StatusSession)
	if !ok {
		return nil, nil
	}
	return &planeStatusRun{session: status, config: c}, nil
}

type planeStatusRun struct {
	session     StatusSession
	config      Config
	schemaCache statusSchema
}

func planeStatusID(m db.IssueStatusMapping) (string, error) {
	if !strings.HasPrefix(m.Mapping.ExternalID, "work-item:") {
		return "", blockedStatus("invalid Plane mapping identity")
	}
	return CanonicalID(strings.TrimPrefix(m.Mapping.ExternalID, "work-item:"))
}
func (r *planeStatusRun) ReadStatus(ctx context.Context, m db.IssueStatusMapping) (issuesync.StatusObservation, error) {
	id, err := planeStatusID(m)
	if err != nil {
		return issuesync.StatusObservation{}, statusReadError(err)
	}
	if session, ok := r.session.(*clientSession); ok {
		observed, _, err := session.readStatusCached(ctx, r.config, id, &r.schemaCache)
		return observed, err
	}
	return r.session.ReadStatus(ctx, r.config, id)
}
func (r *planeStatusRun) WriteStatus(ctx context.Context, m db.IssueStatusMapping, desired string, admit func() error) (issuesync.StatusObservation, error) {
	id, err := planeStatusID(m)
	if err != nil {
		return issuesync.StatusObservation{}, statusReadError(err)
	}
	return r.session.WriteStatus(ctx, r.config, id, desired, admit)
}

var _ issuesync.StatusAdapter = (*Adapter)(nil)
