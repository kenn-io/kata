package cron

import "strings"

// Status filters match the daemon's run status vocabulary. Jobs filter on
// their last result, so they also offer never_ran.
var runStatusFilters = []string{"running", "succeeded", "failed", "cancelled", "unknown"}

func (m *model) statusFilters() []string {
	if m.tab == 0 {
		return append(append([]string(nil), runStatusFilters...), "never_ran")
	}
	return runStatusFilters
}

func (m *model) cycleStatus() {
	filters := m.statusFilters()
	next := ""
	for i, status := range filters {
		if status == m.status {
			if i+1 < len(filters) {
				next = filters[i+1]
			}
			break
		}
	}
	if m.status == "" {
		next = filters[0]
	}
	m.status = next
	m.follow = true
	m.rebuild()
}

func (m *model) statusMatches(r row) bool {
	switch {
	case m.status == "":
		return true
	case r.kind == "run":
		return m.data.Runs[r.run].Status == m.status
	case r.kind == "job":
		v := m.indexedJob(r)
		if v == nil {
			return false
		}
		if last := m.jobLastRun(v); last != nil {
			return last.Status == m.status
		}
		// An older daemon that omits last_run cannot prove a job never ran.
		return m.status == "never_ran" && v.entry.LastRunKnown
	}
	return true
}

func statusFilterLabel(status string) string { return strings.ReplaceAll(status, "_", " ") }
