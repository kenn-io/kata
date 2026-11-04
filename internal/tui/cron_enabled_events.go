package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// A relevant change stays latched even after a list refresh completes. The
// project-wide refresh counter also includes runs and unrelated definitions.
func (m Model) invalidateCronEnabled(msg eventReceivedMsg) Model {
	if pending := m.cron.enabled; pending != nil && pending.invalidatedBy(msg) {
		request := *pending
		request.invalidated = true
		m.cron.enabled = &request
	}
	return m
}

func (r cronEnabledRequest) invalidatedBy(msg eventReceivedMsg) bool {
	switch msg.eventType {
	case "sync.reset_required", "project.merged", "project.removed", "project.restored":
		return true
	}
	if strings.HasPrefix(msg.eventType, "cron.job.") || msg.eventType == "project.metadata_updated" {
		return r.cronEventInvalidates(msg)
	}
	if r.parent == "" || msg.issueUID != r.parent {
		return false
	}
	switch msg.eventType {
	case "issue.created", "issue.snapshot", "issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored", "issue.moved":
		return true
	case "issue.updated":
		var payload map[string]jsontext.Value
		if json.Unmarshal(msg.cronPayload, &payload) != nil || payload == nil {
			return true
		}
		for _, key := range []string{"status", "deleted_at", "project_id", "project_uid", "metadata"} {
			if _, changed := payload[key]; changed {
				return true
			}
		}
	case "issue.metadata_updated":
		var payload struct {
			Diff map[string]jsontext.Value `json:"diff"`
		}
		if json.Unmarshal(msg.cronPayload, &payload) != nil || payload.Diff == nil {
			return true
		}
		_, changed := payload.Diff["cron.progress."+r.uid]
		return changed
	}
	return false
}

// cronEventInvalidates covers job definitions and the applicable cutoffs a
// counted resume depends on. Other project metadata leaves the action intact.
func (r cronEnabledRequest) cronEventInvalidates(msg eventReceivedMsg) bool {
	if msg.eventType == "project.metadata_updated" {
		var payload struct {
			Diff map[string]jsontext.Value `json:"diff"`
		}
		if !r.enabled {
			return false
		}
		if json.Unmarshal(msg.cronPayload, &payload) != nil || payload.Diff == nil {
			return true
		}
		_, project := payload.Diff["cron.passover"]
		_, job := payload.Diff["cron.passover."+r.uid]
		return project || job
	}
	var payload struct {
		UID string `json:"uid"`
	}
	return json.Unmarshal(msg.cronPayload, &payload) != nil || payload.UID == "" || payload.UID == r.uid
}
