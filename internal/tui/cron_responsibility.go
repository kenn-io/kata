package tui

import (
	"encoding/json/v2"
	"strings"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

func cronPreferences(options map[string]any, native bool) cron.Responsibility {
	// Generated options are JSON values. An unusable option map supplies no
	// preference; it must never turn into an execution assignment.
	raw, _ := json.Marshal(options)
	return cron.DefinitionResponsibility(raw, native)
}

func cronJobResponsibility(j generated.CronJob) cron.Responsibility {
	def := cron.JobDefinition{Action: cron.Action{Kind: j.Definition.Action.Kind}}
	if j.Definition.Issue != nil {
		def.Issue = &cron.IssuePolicy{Kind: j.Definition.Issue.Kind}
	}
	return cronPreferences(j.Definition.Options, cron.IsRecurringIssueJob(def))
}

func cronRuntime(r cron.Responsibility) string {
	if r.Runtime != nil {
		return *r.Runtime
	}
	if r.RuntimeStatus == "unreported" {
		return "unreported"
	}
	return "unknown"
}

func cronPreferenceSearch(r cron.Responsibility) string {
	return strings.Join([]string{deref(r.Adapter), deref(r.Executor)}, " ")
}

func cronResponsibilityFields(d *cronDoc, author string, r cron.Responsibility) {
	d.field("Author", author+" (creator)")
	d.field("Actor", "unknown (execution responsibility)")
	runtime := cronRuntime(r)
	if r.RuntimeStatus == "daemon" {
		runtime += " (health unknown)"
	}
	d.field("Runtime", runtime)
	if r.Adapter != nil {
		d.field("Adapter preference", *r.Adapter)
	}
	if r.Executor != nil {
		d.field("Executor preference", *r.Executor)
	}
}

func cronRunAttribution(r generated.CronRun) string {
	return "actor " + r.Actor + " · runtime " + cronRuntime(cron.RunResponsibility(r.Actor, r.ExecutorLabel))
}
