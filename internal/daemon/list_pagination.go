package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/pagination"
)

func normalizePageStrings(values []string, lower bool) []string {
	out := slices.Clone(values)
	for i := range out {
		if lower {
			out[i] = strings.ToLower(out[i])
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func listPageHash(cfg ServerConfig, route string, p db.ListAllIssuesParams, due db.DueFilter) string {
	p.Limit = 0
	p.After = nil
	p.Labels = normalizePageStrings(p.Labels, true)
	p.ExcludeLabels = normalizePageStrings(p.ExcludeLabels, true)
	p.AllowedProjectIDs = slices.Clone(p.AllowedProjectIDs)
	slices.Sort(p.AllowedProjectIDs)
	p.AllowedProjectIDs = slices.Compact(p.AllowedProjectIDs)
	p.Meta = slices.Clone(p.Meta)
	slices.SortFunc(p.Meta, func(a, b db.MetaFilter) int {
		return strings.Compare(fmt.Sprintf("%s\x00%t\x00%s", a.Key, a.HasValue, a.Value), fmt.Sprintf("%s\x00%t\x00%s", b.Key, b.HasValue, b.Value))
	})
	return pagination.Fingerprint(struct {
		Instance, Route string
		Filters         db.ListAllIssuesParams
		Due             db.DueFilter `json:",omitzero"`
	}{cfg.DB.InstanceUID(), route, p, due})
}

func listPriorities(priority, maximum string) (*int64, *int64, bool, error) {
	unset := priority == "none"
	if unset && maximum != "" {
		return nil, nil, false, api.NewError(400, "validation", "priority=none and max_priority are mutually exclusive", "", nil)
	}
	var p *int64
	var err error
	if !unset {
		p, err = parsePriorityQuery(priority, "priority")
		if err != nil {
			return nil, nil, false, err
		}
	}
	m, err := parsePriorityQuery(maximum, "max_priority")
	return p, m, unset, err
}

func pageProbeLimit(limit int) (int, error) {
	if limit < 0 || limit == math.MaxInt {
		return 0, api.NewError(400, "validation", "limit must be non-negative and allow a one-row probe", "", nil)
	}
	if limit > 0 {
		return limit + 1, nil
	}
	return 0, nil
}

func pageBoundary(issues []db.Issue, limit int, hash string, stable bool) ([]db.Issue, api.PageMetadata) {
	more := limit > 0 && len(issues) > limit
	page := api.PageMetadata{Complete: !more, Truncated: more}
	if more {
		issues = issues[:limit]
		if stable {
			last := issues[len(issues)-1]
			page.NextCursor = pagination.Encode(pagination.Position{CreatedAt: last.CreatedAt, ID: last.ID}, hash)
		}
	}
	return issues, page
}

func scopedListParams(p db.ListAllIssuesParams) db.ListIssuesParams {
	return db.ListIssuesParams{ProjectID: p.ProjectID, Status: p.Status, Priority: p.Priority, MaxPriority: p.MaxPriority, PriorityUnset: p.PriorityUnset, Limit: p.Limit, OldestFirst: p.OldestFirst, CreatedFirst: p.CreatedFirst, After: p.After, Unowned: p.Unowned, Owner: p.Owner, Labels: p.Labels, ExcludeLabels: p.ExcludeLabels, Meta: p.Meta, IssueScope: p.IssueScope}
}

// parseListDueFilter validates the due_by filter like the browser snapshot's
// local_date and time_zone. The effective zone (request, then daemon, then UTC)
// is stored so it is part of the cursor hash. The zero filter means no due_by.
func parseListDueFilter(cfg ServerConfig, dueBy, timeZone string) (db.DueFilter, error) {
	dueBy, timeZone = strings.TrimSpace(dueBy), strings.TrimSpace(timeZone)
	if dueBy == "" {
		if timeZone != "" {
			return db.DueFilter{}, uiValidationError("time_zone", "time_zone requires due_by", nil)
		}
		return db.DueFilter{}, nil
	}
	if !metadata.ValidCalendarDate(dueBy) {
		return db.DueFilter{}, uiValidationError("due_by", "due_by must use YYYY-MM-DD", nil)
	}
	if timeZone != "" && !metadata.ValidTimeZone(timeZone) {
		return db.DueFilter{}, uiValidationError("time_zone", "time_zone must be a valid IANA timezone", nil)
	}
	if timeZone == "" {
		timeZone = cfg.DefaultTimezone
	}
	if timeZone == "" {
		timeZone = "UTC"
	}
	return db.DueFilter{By: dueBy, TimeZone: timeZone, DefaultTimezone: cfg.DefaultTimezone}, nil
}

// dueRows maps an issue id to its row of a due_by list.
type dueRows map[int64]db.DueIssue

// listPlainOrDue runs plain for ordinary lists and ListDueIssues when due is
// set, returning the matched rows' projected dates alongside them.
func listPlainOrDue(
	ctx context.Context, store db.Storage, params db.ListAllIssuesParams, due db.DueFilter,
	plain func() ([]db.Issue, error),
) ([]db.Issue, dueRows, error) {
	if due == (db.DueFilter{}) {
		issues, err := plain()
		return issues, nil, err
	}
	rows, err := store.ListDueIssues(ctx, params, due)
	if err != nil {
		return nil, nil, err
	}
	issues := make([]db.Issue, len(rows))
	byID := make(dueRows, len(rows))
	for i, row := range rows {
		issues[i] = row.Issue
		byID[row.ID] = row
	}
	return issues, byID, nil
}

// countPlainOrDue is listPlainOrDue for the include_total count.
func countPlainOrDue(
	ctx context.Context, store db.Storage, params db.ListAllIssuesParams, due db.DueFilter,
	plain func() (int64, error),
) (int64, error) {
	if due == (db.DueFilter{}) {
		return plain()
	}
	return store.CountDueIssues(ctx, params, due)
}

func attachDueDates(outs []api.IssueOut, rows dueRows) {
	if rows == nil {
		return
	}
	for i := range outs {
		row := rows[outs[i].ID]
		outs[i].ScheduledOnDate = row.ScheduledOnDate
		outs[i].DeadlineOnDate = row.DeadlineOnDate
	}
}
