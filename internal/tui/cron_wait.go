package tui

import (
	"encoding/json/v2"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
)

func cronRunWaitForActor(wait cronParent, actor string) cronHealthState {
	out := cronHealthState{"running", ""}
	if !wait.open {
		return out
	}
	if attention, _ := wait.metadata["work.attention"].(string); attention == "needs-human" || attention == "stuck" {
		out = cronHealthState{"blocked", "Needs human attention"}
		if attention == "stuck" {
			out.reason = "Stuck"
		}
	}
	recipient, err := notification.NormalizeRecipient(actor)
	if err != nil {
		return out
	}
	raw, err := json.Marshal(wait.metadata[notification.MetadataKey(recipient)])
	if err != nil {
		return out
	}
	request, _, err := notification.DecodeValue(raw)
	if err == nil {
		reason := "waiting on " + recipient + ": " + request.Message
		if out.state == "blocked" {
			reason += "; " + out.reason
		}
		out = cronHealthState{"blocked", truncate(sanitizeForLine(reason), 240)}
	}
	return out
}

func cronRunHealth(p cronData, r generated.CronRun) cronHealthState {
	out := cronHealthState{r.Status, ""}
	if r.Status != "running" {
		return out
	}
	if j := p.job(deref(r.JobUID)); j != nil && cronNativeParent(*j) != "" {
		return out
	}
	uid := deref(r.IssueUID)
	if uid == "" {
		return out
	}
	wait, ok := p.waits[uid]
	if !ok || p.waitErr[uid] != nil {
		return cronHealthState{"running", "attention unavailable"}
	}
	return cronRunWaitForActor(wait, r.Actor)
}

func cronRunStatus(p cronData, r generated.CronRun) string {
	if cronRunHealth(p, r).state == "blocked" {
		return "running · blocked"
	}
	return r.Status
}
