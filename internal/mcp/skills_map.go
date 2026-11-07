package mcpserver

import (
	"slices"
	"strings"
)

// SkillCommand maps one CLI command path to its MCP tools and semantic differences.
type SkillCommand struct {
	CLI   string
	Tools []string
	Note  string
}

var skillCommands = []SkillCommand{
	{"kata search", []string{"kata.search"}, "`project` narrows the search; omit it to search every project in scope"},
	{"kata create", []string{"kata.create"}, "`idempotency_key` required; `project` required in multi-project scope"},
	{"kata show", []string{"kata.show"}, ""},
	{"kata comment", []string{"kata.comment"}, "`idempotency_key` required"},
	{"kata edit", []string{"kata.edit"}, "relationship flags are fields; field and metadata changes need separate calls"},
	{"kata label add", []string{"kata.set_label"}, "`present: true`; `--comment` becomes a separate `kata.comment` call"},
	{"kata next", []string{"kata.next"}, ""},
	{"kata ready", []string{"kata.ready"}, ""},
	{"kata claim", []string{"kata.claim"}, ""},
	{"kata unassign", []string{"kata.unassign"}, "`--expect-owner` maps to `expected_owner`"},
	{"kata status", []string{"kata.status"}, ""},
	{"kata meta set", []string{"kata.set_metadata"}, "`patch: {\"key\": value}`"},
	{"kata meta unset", []string{"kata.set_metadata"}, "set the key to JSON `null`"},
	{"kata schedule", []string{"kata.set_schedule"}, "`-` maps to `clear_schedule: true`"},
	{"kata deadline", []string{"kata.set_deadline"}, "`-` maps to `clear_deadline: true`"},
	{"kata close", []string{"kata.close"}, "`reason` required; evidence is a typed list such as `[{\"type\":\"commit\",\"sha\":\"…\"}]`"},
	{"kata reopen", []string{"kata.reopen"}, ""},
	{"kata audit closes", []string{"kata.audit_closes"}, ""},
	{"kata delete", []string{"kata.delete"}, "exact confirmation required"},
	{"kata purge", []string{"kata.purge"}, "exact confirmation required"},
	{"kata inbox", []string{"kata.inbox"}, "`--for` maps to `for`; lists `notify.*` requests on open issues, not `work.attention`"},
	{"kata wait", []string{"kata.wait"}, "`kata.wait` waits on issue status only. For `--until attention`, poll `kata.show` for each watched ref with your own deadline: closed completes first, then a non-empty `work.attention` other than `ok`. Notifications are separate state."},
	{"kata notify", nil, "no MCP tool yet; an operator with CLI access must send or clear requests"},
	{"kata events", []string{"kata.events"}, ""},
	{"kata cron guide", []string{"kata.skills"}, "`name: \"cron\"`; always available, no loader needed"},
	{"kata tui", nil, "CLI only; Jobs previews native Next and nominal adapter Triggers"},
	{"kata cron capabilities", []string{"kata.cron_capabilities"}, "`project` required outside a bound single project"},
	{"kata cron job list", []string{"kata.cron_jobs"}, "`--include-deleted` maps to `include_deleted`"},
	{"kata cron job show", []string{"kata.cron_job"}, "`uid` is the full job ULID"},
	{"kata cron job create", []string{"kata.cron_new_uid", "kata.cron_job_create"}, "retain the generated `uid` before create and reuse on retry; resolve `--parent` to a full UID in `definition.issue.parent_uid`"},
	{"kata cron job update", []string{"kata.cron_job_update"}, "replace the full definition; `--expected-event-uid` maps to `expected_event_uid`"},
	{"kata cron job delete", []string{"kata.cron_job_delete"}, "reversible archive; current `expected_event_uid` required"},
	{"kata cron job restore", []string{"kata.cron_job_restore"}, "current `expected_event_uid` required"},
	{"kata cron workflow list", []string{"kata.cron_workflows"}, "`--include-deleted` maps to `include_deleted`"},
	{"kata cron workflow show", []string{"kata.cron_workflow"}, "`uid` is the full workflow ULID"},
	{"kata cron workflow create", []string{"kata.cron_new_uid", "kata.cron_workflow_create"}, "retain the generated `uid` before create and reuse on retry"},
	{"kata cron run list", []string{"kata.cron_runs"}, "`--job-uid` maps to `job_uid`; observations never launch work"},
	{"kata cron run show", []string{"kata.cron_run"}, "`uid` is the full run ULID; corroborate with adapter output"},
}

// SkillCommands returns independent copies of every command row in table order.
func SkillCommands() []SkillCommand {
	rows := slices.Clone(skillCommands)
	for i := range rows {
		rows[i].Tools = slices.Clone(rows[i].Tools)
	}
	return rows
}

// SkillGuideCommands returns the CLI command keys rendered by a guide, in order.
func SkillGuideCommands(name string) []string {
	if name == "workflow" {
		keys := make([]string, len(skillCommands))
		for i, row := range skillCommands {
			keys[i] = row.CLI
		}
		return keys
	}
	for _, guide := range skillGuides {
		if guide.name == name {
			return slices.Clone(guide.commands)
		}
	}
	return nil
}

func sectionLoaderForTool(tool string) (string, bool) {
	for _, section := range toolSections(true, true) {
		if slices.Contains(section.tools, tool) {
			return section.loader, true
		}
	}
	return "", false
}

func renderCommandRow(c SkillCommand, loader string) string {
	escape := func(cell string) string { return strings.ReplaceAll(cell, "|", `\|`) }
	tools, load := "none", "—"
	if len(c.Tools) != 0 {
		tools = "`" + strings.Join(c.Tools, "`, `") + "`"
		if loader != "" {
			load = "`" + loader + "`"
		}
	}
	return "| `" + escape(c.CLI) + "` | " + escape(tools) + " | " + escape(load) + " | " + escape(c.Note) + " |"
}

func renderSkillCommands(name string) string {
	var b strings.Builder
	b.WriteString("## MCP tools for this guide\n\n| CLI command | MCP tool | Load with | Note |\n| --- | --- | --- | --- |\n")
	for _, key := range SkillGuideCommands(name) {
		for _, row := range skillCommands {
			if row.CLI != key {
				continue
			}
			loader := ""
			if len(row.Tools) != 0 {
				loader, _ = sectionLoaderForTool(row.Tools[0])
			}
			b.WriteString(renderCommandRow(row, loader) + "\n")
		}
	}
	return b.String()
}
