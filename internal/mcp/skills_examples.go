package mcpserver

import (
	"encoding/json/v2"
	"maps"
	"strings"
)

type skillExample struct {
	Step  string
	Tool  string
	Bound map[string]any
	Multi map[string]any
}

var skillExamples = []skillExample{
	{Step: "Search first", Tool: "kata.search", Bound: map[string]any{"query": "login race"}},
	{Step: "Create when nothing fits", Tool: "kata.create",
		Bound: map[string]any{"title": "Fix login race", "idempotency_key": "login-race-2026-10-25"},
		Multi: map[string]any{"title": "Fix login race", "idempotency_key": "login-race-2026-10-25", "project": "spoke-project"}},
	issueSkillExample("Claim", "kata.claim", nil),
	issueSkillExample("Mark tracked", "kata.set_metadata", map[string]any{"patch": map[string]any{"work.attention": "ok"}}),
	issueSkillExample("Report a blocker", "kata.set_metadata", map[string]any{"patch": map[string]any{"work.attention": "stuck", "work.attention_msg": "Waiting for a reproducible failing request."}}),
	issueSkillExample("Request human input", "kata.set_metadata", map[string]any{"patch": map[string]any{"work.attention": "needs-human", "work.attention_msg": "Please choose the expected retry behavior."}}),
	issueSkillExample("Record progress", "kata.comment", map[string]any{"body": "Found the duplicate-submit path; working on the retry guard.", "idempotency_key": "login-race-progress-2026-10-25"}),
	issueSkillExample("Watch delegated work", "kata.show", nil),
	issueSkillExample("Close with evidence", "kata.close", map[string]any{
		"reason": "done", "message": "Fixed duplicate login submissions and checked the retry regression.",
		"evidence": []any{map[string]any{"type": "commit", "sha": "0123456789abcdef0123456789abcdef01234567"}},
	}),
	issueSkillExample("Hand back incomplete work instead of closing", "kata.set_label", map[string]any{"label": "needs-review", "present": true}),
	issueSkillExample("Explain what remains", "kata.comment", map[string]any{"body": "The retry path needs another reproduction before this can be completed.", "idempotency_key": "login-race-handoff-2026-10-25"}),
}

func issueSkillExample(step, tool string, fields map[string]any) skillExample {
	bound := map[string]any{"ref": "abc4"}
	maps.Copy(bound, fields)
	multi := maps.Clone(bound)
	multi["ref"] = "spoke-project#abc4"
	return skillExample{Step: step, Tool: tool, Bound: bound, Multi: multi}
}

func renderSkillExamples() string {
	var b strings.Builder
	b.WriteString("## MCP calls for the required steps\n\nUse the bound form for one project. Use the multi-project form where shown.\nFor delegated attention, poll the show call with your own deadline: closed\ncompletes first, then non-empty work.attention other than ok.\n\n")
	for _, example := range skillExamples {
		b.WriteString("### " + example.Step + "\n\n")
		for i, form := range []map[string]any{example.Bound, example.Multi} {
			if form == nil {
				continue
			}
			if i == 1 {
				b.WriteString("Multi-project: ")
			}
			args, err := json.Marshal(form, json.Deterministic(true))
			if err != nil {
				panic(err) // Examples contain only fixed JSON values.
			}
			b.WriteString("`" + example.Tool + " " + string(args) + "`\n\n")
		}
	}
	return b.String()
}
