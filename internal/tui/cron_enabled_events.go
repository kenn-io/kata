package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// A relevant change stays latched even after a list refresh completes. The
// project-wide refresh counter also includes runs and unrelated definitions.
func (m Model) invalidateCronAction(msg eventReceivedMsg) Model {
	if pending := m.cron.action; pending != nil && pending.invalidatedBy(msg) {
		action := *pending
		action.invalidated = true
		m.cron.action = &action
	}
	return m
}

// invalidatedBy reports whether a pause or resume depends on what msg changed:
// the selected job, its parent's status or progress, or the cutoffs a counted
// resume applies. An unclassifiable change invalidates. Runtime requests are
// fenced by their own live read instead.
func (a cronPending) invalidatedBy(msg eventReceivedMsg) bool {
	if a.request() {
		return false
	}
	var diff struct {
		Diff map[string]jsontext.Value `json:"diff"`
	}
	diffKey := func(keys ...string) bool {
		if json.Unmarshal(msg.cronPayload, &diff) != nil || diff.Diff == nil {
			return true
		}
		for _, key := range keys {
			if _, changed := diff.Diff[key]; changed {
				return true
			}
		}
		return false
	}
	switch {
	case msg.eventType == "sync.reset_required", msg.eventType == "project.merged", msg.eventType == "project.removed", msg.eventType == "project.restored":
		return true
	case strings.HasPrefix(msg.eventType, "cron.job."):
		var job struct {
			UID string `json:"uid"`
		}
		return json.Unmarshal(msg.cronPayload, &job) != nil || job.UID == "" || job.UID == a.target
	case msg.eventType == "project.metadata_updated":
		return a.kind == cronResume && diffKey("cron.passover", "cron.passover."+a.target)
	case a.parent == "" || msg.issueUID != a.parent:
		return false
	case msg.eventType == "issue.metadata_updated":
		return diffKey("cron.progress." + a.target)
	case msg.eventType == "issue.updated":
		var fields map[string]jsontext.Value
		if json.Unmarshal(msg.cronPayload, &fields) != nil || fields == nil {
			return true
		}
		for _, key := range []string{"status", "deleted_at", "project_id", "project_uid", "metadata"} {
			if _, changed := fields[key]; changed {
				return true
			}
		}
		return false
	}
	switch msg.eventType {
	case "issue.created", "issue.snapshot", "issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored", "issue.moved":
		return true
	}
	return false
}
