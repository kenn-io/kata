package githubsync

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func (a *adapter) OpenStatus(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (issuesync.StatusRun, error) {
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, err
	}
	if b.Provider != "github" {
		return nil, fmt.Errorf("github status requires a GitHub binding")
	}
	fetcher, err := a.fetcherForBinding(ctx, c.Binding())
	if err != nil {
		return nil, err
	}
	status, ok := fetcher.(StatusSession)
	if !ok {
		return nil, nil
	}
	locatorSession, _ := fetcher.(StatusLocatorSession)
	return &githubStatusRun{session: status, config: c, locators: locatorSession}, nil
}

type githubStatusRun struct {
	session  StatusSession
	config   Config
	locators StatusLocatorSession
}

func statusMappingNumber(m db.IssueStatusMapping) (int, error) {
	number, err := strconv.Atoi(m.State.RemoteLocator)
	if err != nil || number <= 0 || strconv.Itoa(number) != m.State.RemoteLocator {
		return 0, &issuesync.StatusError{Message: "GitHub issue API locator is not yet available", Blocked: true}
	}
	return number, nil
}
func (r *githubStatusRun) ReadStatus(ctx context.Context, m db.IssueStatusMapping) (issuesync.StatusObservation, error) {
	number, err := statusMappingNumber(m)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	return r.session.ReadStatus(ctx, r.config, m.Mapping.ExternalID, number)
}
func (r *githubStatusRun) WriteStatus(ctx context.Context, m db.IssueStatusMapping, desired string, admit func() error) (issuesync.StatusObservation, error) {
	number, err := statusMappingNumber(m)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	return r.session.WriteStatus(ctx, r.config, m.Mapping.ExternalID, number, desired, admit)
}

var _ issuesync.StatusAdapter = (*adapter)(nil)

func (r *githubStatusRun) Locators(ctx context.Context, page int) ([]db.IssueStatusLocator, int, error) {
	if r.locators == nil {
		return nil, 0, nil
	}
	rows, next, err := r.locators.StatusLocators(ctx, r.config, page)
	if err != nil {
		return nil, 0, err
	}
	locators := make([]db.IssueStatusLocator, 0, len(rows))
	for _, row := range rows {
		locators = append(locators, db.IssueStatusLocator{ExternalID: row.ExternalID, LegacyExternalIDs: row.LegacyExternalIDs, Locator: strconv.Itoa(row.Number)})
	}
	return locators, next, nil
}
